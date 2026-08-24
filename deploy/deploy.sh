#!/usr/bin/env bash
# Выкат стека на сервере.
#
# Запускается в каталоге окружения, где лежат compose.yml, smoke.sh,
# config/ и .env. Порядок продиктован тем, что откатывается легко,
# а что не откатывается вовсе:
#
#   1. образы тянутся заранее — обрыв связи не должен застать выкат
#      на середине;
#   2. миграции применяются отдельным шагом, до подмены образов: сбой
#      миграции обязан остановить выкат, а не оставить половину сервисов
#      на новой схеме;
#   3. стек поднимается и ждёт проб;
#   4. дымовой сценарий; если он не прошёл — образы возвращаются
#      на предыдущий тег.
#
# Схема при откате не трогается: откат миграции возможен не всегда
# (восстановить ограничение уникальности поверх накопившихся данных
# уже нельзя), и делать это молча в аварийной ситуации нельзя тем более.
set -euo pipefail

cd "$(dirname "$0")"

TAG="${1:-${TAG:-}}"
case "$TAG" in
"" | -*)
    echo "использование: $0 <тег образа>" >&2
    echo "тег — это то, что выкатывается: sha-<полный SHA коммита> или v1.2.3" >&2
    exit 1
    ;;
esac

# Имя проекта и файл стека передаются дальше через окружение: тот же
# набор видит и docker compose, и дымовой сценарий. Два окружения на одной
# машине различаются именно именем проекта и портами.
export COMPOSE_FILE="${COMPOSE_FILE:-compose.yml}"
export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-caldero}"
export GATEWAY_URL="${GATEWAY_URL:-http://localhost:${GATEWAY_PORT:-8080}}"
export WEB_URL="${WEB_URL:-http://localhost:${WEB_PORT:-3000}}"

SMOKE="${SMOKE:-./smoke.sh}"
# Файл помнит, что выкачено сейчас: без него откатываться некуда.
STATE="${STATE:-.deployed-tag}"
# Сервисы, у которых есть своя схема. Миграции лежат в самом образе,
# поэтому применяет их тот же образ, запущенный с флагом -migrate.
MIGRATED="${MIGRATED:-wallet credit account users notify wishlist caldron}"
# PULL=0 отключает тягу образов. Нужно ровно для одного случая: проверить
# сам выкат на локально собранных образах, когда реестра под рукой нет.
PULL="${PULL:-1}"

compose() { TAG="$1" docker compose "${@:2}"; }

previous=""
[ -f "$STATE" ] && previous=$(cat "$STATE")

echo "== выкат $TAG (предыдущий: ${previous:-нет})"

if [ "$PULL" = "1" ]; then
    echo "== образы"
    compose "$TAG" pull --quiet
fi

echo "== база и кэш"
compose "$TAG" up -d --wait postgres-db redis

echo "== миграции"
for service in $MIGRATED; do
    echo "-- $service"
    if ! compose "$TAG" run --rm --no-deps "$service" -migrate; then
        # Образы ещё не подменены: работает прежняя версия, и откатывать
        # нечего. Выкат просто не состоялся — это и нужно сказать.
        echo "миграция $service не прошла, выкат остановлен; работает ${previous:-прежняя версия}" >&2
        exit 1
    fi
done

echo "== запуск"
if compose "$TAG" up -d --wait --remove-orphans; then
    if "$SMOKE"; then
        echo "$TAG" >"$STATE"
        echo "== выкачено: $TAG"
        exit 0
    fi
    echo "дымовой сценарий не прошёл" >&2
else
    echo "стек не поднялся" >&2
fi

if [ -z "$previous" ] || [ "$previous" = "$TAG" ]; then
    echo "откатываться некуда: предыдущего выката нет" >&2
    exit 1
fi

echo "== откат на $previous"
# Откатываются только образы. Если новая версия успела применить миграцию,
# схема останется новой — и об этом нужно знать, а не догадываться.
echo "ВНИМАНИЕ: схема осталась на версии от $TAG, откат схемы вручную" >&2
compose "$previous" up -d --wait --remove-orphans
if "$SMOKE"; then
    echo "== откачено на $previous"
else
    echo "откат не помог: стек нездоров" >&2
fi
exit 1
