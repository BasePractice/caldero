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
# X-Roles там быть не должно: роли в токен пока не попадают, перезаписать
# присланное клиентом нечем, и любой прислал бы себе роль сам. Заголовок
# с идентификатором так не подделать — claim sub есть всегда, и шлюз
# перезаписывает им присланное значение.
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
    if "X-Roles" in headers:
        problems.append(f"{route}: X-Roles передаётся от клиента — это чужая роль по своему желанию")

for problem in problems:
    print(f"  ✗ {problem}")
sys.exit(1 if problems else 0)
PYTHON
    failed=1
else
    echo "  передаётся"
fi

echo "== токен доходит до сервиса пользователей"
# Сервис пользователей проверяет токен сам: он и есть провайдер, и доверять
# заголовку X-Authorized-Id ему нечего — он его и выдаёт. Поэтому его
# защищённым маршрутам нужен заголовок Authorization, а не только claim.
# Без него шлюз отвечал 401 на верный токен, и после входа не работало
# ничего: ни профиль, ни подтверждение контактов.
if ! python3 - "$rendered/internal.json" <<'PYTHON'; then
import json, sys

with open(sys.argv[1]) as source:
    config = json.load(source)

missing = []
for endpoint in config["endpoints"]:
    backends = [host for backend in endpoint["backend"] for host in backend["host"]]
    protected = "auth/validator" in endpoint.get("extra_config", {})
    if not protected or not any("users" in host for host in backends):
        continue
    if "Authorization" not in endpoint.get("input_headers", []):
        missing.append(f"{endpoint['method']} {endpoint['endpoint']}")

for route in missing:
    print(f"  ✗ {route}: сервису пользователей не передаётся Authorization")
sys.exit(1 if missing else 0)
PYTHON
    failed=1
else
    echo "  передаётся"
fi

[ "$failed" = 0 ] || {
    echo "шлюз не отдаёт маршруты, без которых система не работает" >&2
    exit 1
}
echo "конфигурация шлюза в порядке"
