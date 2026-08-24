#!/usr/bin/env bash
# Дымовой сценарий: проверяет поднятый стенд снаружи и изнутри.
#
# Запускается и локально после `make up`, и на сервере после выката — тем же
# кодом: проверка, которая существует только в доставке, к моменту выката
# оказывается непроверенной сама.
#
# Что проверяется:
#   1. контейнеры стека живы (проба образа — соединение с портом сервиса);
#   2. каждый сервис отвечает readyz, то есть видит свои зависимости;
#   3. публичный маршрут проходит целиком: шлюз -> users -> база;
#   4. интерфейс отдаёт страницу.
#
# Какой стенд проверять, задаётся переменными docker compose:
#   COMPOSE_FILE=deploy/compose.yml scripts/smoke.sh
set -euo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
WEB_URL="${WEB_URL:-http://localhost:3000}"
# Проверка сертификата отключается только на стенде, где его выдал
# внутренний удостоверяющий центр Caddy: настоящему имени он получает
# обычный сертификат, и там проверка обязана работать.
INSECURE=""
[ "${SMOKE_INSECURE:-0}" = "1" ] && INSECURE="--insecure"
# Сервисы с пробой готовности. web не ходит в базу, но readyz у него тот же.
SERVICES="${SMOKE_SERVICES:-wallet credit account users notify wishlist caldron web}"
# Остальные контейнеры стека: у них проверяется только состояние.
# В окружении сюда добавляется прокси, на локальном стенде его нет.
INFRA="${SMOKE_INFRA:-krakend postgres-db redis}"
# Метрики и пробы живут на служебном порту, наружу он не публикуется —
# отсюда и проверка изнутри сети.
METRICS_PORT="${METRICS_PORT:-8081}"
# Образ с curl: в собранных из scratch сервисах нет ни оболочки, ни curl.
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.11.1}"

fail() {
    echo "дымовой сценарий не прошёл: $1" >&2
    exit 1
}

# Сеть стенда берётся у поднятого контейнера, а не собирается из имени
# проекта: имя зависит от каталога запуска и от флага -p.
container=$(docker compose ps -q krakend 2>/dev/null || true)
[ -n "$container" ] || fail "шлюз не запущен"
network=$(docker inspect -f '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}} {{end}}' "$container" | awk '{print $1}')
[ -n "$network" ] || fail "не удалось определить сеть стенда"

echo "== состояние контейнеров"
for service in $SERVICES $INFRA; do
    id=$(docker compose ps -q "$service" 2>/dev/null || true)
    [ -n "$id" ] || fail "сервис $service не запущен"

    state=$(docker inspect -f '{{.State.Status}}' "$id")
    [ "$state" = "running" ] || fail "сервис $service в состоянии $state"

    # Проба есть не у всех образов: её отсутствие не отказ, а отказавшая
    # проба — отказ.
    health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$id")
    case "$health" in
    healthy | none) ;;
    *) fail "сервис $service: проба отвечает $health" ;;
    esac
    printf '  %-12s %s\n' "$service" "${health}"
done

echo "== готовность сервисов"
for service in $SERVICES; do
    if ! docker run --rm --network "$network" "$CURL_IMAGE" \
        --fail --silent --show-error --max-time 10 \
        "http://$service:$METRICS_PORT/readyz" >/dev/null; then
        fail "сервис $service не готов: readyz ответил отказом"
    fi
    printf '  %-12s готов\n' "$service"
done

echo "== публичный маршрут через шлюз"
# JWKS выбран намеренно: маршрут открытый, ничего не меняет и проходит
# весь путь целиком — шлюз, сервис пользователей и его база.
# shellcheck disable=SC2086 # INSECURE — либо пустая строка, либо один флаг
jwks=$(curl --fail --silent --show-error --max-time 15 $INSECURE \
    "$GATEWAY_URL/api/v1/.well-known/jwks.json") || fail "шлюз не отдал JWKS"
case "$jwks" in
*'"keys"'*) echo "  JWKS отдан" ;;
*) fail "в ответе JWKS нет ключей: $jwks" ;;
esac

echo "== интерфейс"
# shellcheck disable=SC2086 # INSECURE — либо пустая строка, либо один флаг
curl --fail --silent --show-error --max-time 15 $INSECURE --output /dev/null "$WEB_URL/" ||
    fail "интерфейс не отдал страницу"
echo "  страница отдана"

echo "дымовой сценарий пройден"
