#!/usr/bin/env bash
# Проверяет конфигурацию шлюза: рендерится ли шаблон в обоих режимах
# провайдера и на месте ли маршруты, без которых система не работает.
#
# Рендер сам по себе мало что доказывает: шаблон рендерился и тогда, когда
# из-за отсутствующего маршрута вход через интерфейс не начинался вовсе.
#
# Страницы входа в этом списке нет намеренно: она идёт мимо шлюза, потому
# что KrakenD Community Edition сам ходит по перенаправлению бэкенда
# и не отдаёт клиенту 30x (EXT-10). Подробности — в docs/requirements.md.
set -euo pipefail

IMAGE="${KRAKEND_IMAGE:-krakend:latest}"
TEMPLATE="config/krakend/krakend.tmpl"

# Маршруты, без которых интерфейс нерабочий: то, что он зовёт сразу после
# входа. Список неполный намеренно — иначе проверка превратится в копию
# конфигурации; сюда попадает то, отсутствие чего ломает сценарий целиком.
# Именно так и вышло: /api/v1/profile в шлюзе не было, и вход заканчивался
# пустым экраном.
REQUIRED="
/api/v1/token POST
/api/v1/register POST
/api/v1/.well-known/jwks.json GET
/api/v1/profile GET
/api/v1/wishlist/items GET
/api/v1/notify/messages GET
"

rendered=$(mktemp -d)
trap 'rm -rf "$rendered"' EXIT
failed=0

render() {
    local mode=$1 host=$2 jwks=$3 token=$4 roles=$5
    docker run --rm \
        -e FC_ENABLE=1 -e FC_OUT=/out/"$mode".json \
        -e KRAKEND_NAME=Caldero \
        -e AUTH_HOST="$host" -e AUTH_JWKS_PATH="$jwks" \
        -e AUTH_TOKEN_PATH="$token" \
        -e AUTH_ROLES_CLAIM="$roles" \
        -v "$PWD/config/krakend":/etc/krakend \
        -v "$rendered":/out \
        "$IMAGE" check -c /etc/krakend/krakend.tmpl >/dev/null
}

echo "== рендер шаблона"
render keycloak http://keycloak:8080 \
    /realms/krakend/protocol/openid-connect/certs \
    /realms/krakend/protocol/openid-connect/token \
    realm_access.roles
echo "  keycloak"
render internal http://users:51053 /.well-known/jwks.json /token roles
echo "  internal"

echo "== обязательные маршруты"
while read -r endpoint method; do
    [ -n "$endpoint" ] || continue
    # Рендер обоих режимов даёт один и тот же набор маршрутов: режим
    # меняет адреса бэкендов, а не то, что открыто наружу.
    #
    # Пробелы и переводы строк убираются: рендер печатает JSON с отступами,
    # а искать пару «адрес и метод» надо рядом — иначе метод соседнего
    # маршрута сойдёт за свой.
    if tr -d ' \n' <"$rendered/internal.json" |
        grep -q "\"endpoint\":\"$endpoint\",\"method\":\"$method\""; then
        printf '  %-6s %s\n' "$method" "$endpoint"
    else
        echo "  ✗ $method $endpoint отсутствует в конфигурации шлюза" >&2
        failed=1
    fi
done <<EOF
$REQUIRED
EOF

echo "== пользователь доходит до сервисов"
# Заголовок с идентификатором проставляет сам шлюз, но до бэкенда он
# доходит, только если перечислен в input_headers: фильтр срабатывает
# и на заголовки шлюза. Без него сервисы отвечали 401 на верный токен —
# то есть через шлюз не работало ничего, кроме публичных маршрутов.
#
# Подделать эти заголовки нельзя: шлюз перезаписывает присланное клиентом
# значениями claim. Держится это на том, что claim есть всегда — sub
# по определению, а роли выдаются каждому, пусть и одной только ролью
# user. Пустой claim означал бы, что перезаписывать нечем и любой прислал
# бы себе admin сам.
if ! python3 - "$rendered/internal.json" <<'PYTHON'; then
import json, sys

with open(sys.argv[1]) as source:
    config = json.load(source)

problems = []
for endpoint in config["endpoints"]:
    if "auth/validator" not in endpoint.get("extra_config", {}):
        continue
    route = f"{endpoint['method']} {endpoint['endpoint']}"
    headers = endpoint.get("input_headers", [])
    if "X-Authorized-Id" not in headers:
        problems.append(f"{route}: сервису не передаётся X-Authorized-Id")
    if "X-Roles" not in headers:
        problems.append(f"{route}: сервису не передаётся X-Roles")

for problem in problems:
    print(f"  ✗ {problem}")
sys.exit(1 if problems else 0)
PYTHON
    failed=1
else
    echo "  передаётся"
fi

echo "== токен доходит туда, где его проверяют"
# Сервис пользователей — сам провайдер: часть его обработчиков опознаёт
# вызывающего введением токена (service.protect), а не заголовком, который
# сам же и выдал. Таким маршрутам нужен Authorization, иначе шлюз отвечал
# 401 на верный токен — так и было, пока заголовок не передавался никому.
#
# Список таких маршрутов не ведётся руками: он читается из самого сервиса.
if ! python3 - "$rendered/internal.json" <<'PYTHON'; then
import json, os, re, sys

protected = set()
directory = "services/cmd/users"
for name in sorted(os.listdir(directory)):
    if not name.endswith(".go") or name.endswith("_test.go"):
        continue
    with open(os.path.join(directory, name)) as source:
        for match in re.finditer(
                r'mux\.HandleFunc\("([A-Z]+) ([^"]+)",\s*\w+\.protect\(', source.read()):
            protected.add((match.group(1), match.group(2)))

with open(sys.argv[1]) as source:
    config = json.load(source)

missing = []
for endpoint in config["endpoints"]:
    route = (endpoint["method"], endpoint["endpoint"].replace("/api/v1", ""))
    if route not in protected:
        continue
    if "Authorization" not in endpoint.get("input_headers", []):
        missing.append(f"{route[0]} {endpoint['endpoint']}")

for route in missing:
    print(f"  ✗ {route}: обработчик вводит токен, а шлюз его не передаёт")
sys.exit(1 if missing else 0)
PYTHON
    failed=1
else
    echo "  передаётся"
fi

echo "== маршруты сервисов выставлены наружу"
# Шлюз — единственная дверь, и маршрут, которого в нём нет, для внешнего
# мира не существует. Так уже случалось: сервис умел отдавать профиль
# и карточку пользователя, а через шлюз их было не получить.
#
# Список внутренних маршрутов ведётся руками и объясняет каждый: наружу
# выставлено не всё, и это осознанно.
if ! python3 - "$rendered/internal.json" <<'PYTHON'; then
import json, os, re, sys

# Наружу не выставлены намеренно. Ключ — «метод путь» у сервиса.
INTERNAL = {
    "GET /auth": "страница входа идёт через сервис интерфейса: шлюз не пропускает перенаправление с кодом (EXT-10)",
    "POST /auth": "то же для отправки формы входа",
    "POST /clients": "административный маршрут под отдельным токеном",
    "POST /revoke": "административный маршрут под отдельным токеном",
    "POST /rotate-keys": "административный маршрут под отдельным токеном",
    "GET /me": "сведения о токене; интерфейсу не нужны, наружу не выставлены",
    "GET /users/{id}/contacts": "контакты пользователя — персональные данные, ходит только сервис оповещений",
    "GET /notify/ws": "KrakenD Community Edition не проксирует WebSocket (EXT-05)",
}

with open(sys.argv[1]) as source:
    gateway = {
        (endpoint["method"], endpoint["endpoint"].replace("/api/v1", ""))
        for endpoint in json.load(source)["endpoints"]
    }

missing = []
for service in sorted(os.listdir("services/cmd")):
    # Сервис интерфейса стоит не за шлюзом, а рядом с ним: браузер ходит
    # к нему напрямую за статикой и страницей входа.
    if service == "web":
        continue
    directory = os.path.join("services/cmd", service)
    for name in sorted(os.listdir(directory)):
        if not name.endswith(".go") or name.endswith("_test.go"):
            continue
        with open(os.path.join(directory, name)) as source:
            for match in re.finditer(r'mux\.HandleFunc\("([A-Z]+) ([^"]+)"', source.read()):
                route = (match.group(1), match.group(2))
                if route in gateway or f"{route[0]} {route[1]}" in INTERNAL:
                    continue
                missing.append(f"{service}: {route[0]} {route[1]}")

for route in sorted(set(missing)):
    print(f"  ✗ {route} — наружу не выставлен и не отмечен как внутренний")
sys.exit(1 if missing else 0)
PYTHON
    failed=1
else
    echo "  выставлены"
fi

[ "$failed" = 0 ] || {
    echo "шлюз не отдаёт маршруты, без которых система не работает" >&2
    exit 1
}
echo "конфигурация шлюза в порядке"
