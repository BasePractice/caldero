#!/usr/bin/env bash
# Прогоняет миграции всех сервисов вперёд и назад на временном PostgreSQL.
# Ловит то, чего не видит компилятор: порядок удаления таблиц, конфликты
# между соседними версиями, невозможность отката.
set -euo pipefail

SERVICES="wallet credit account users"
CONTAINER="caldero-migrations-$$"
IMAGE="postgres:16-alpine"

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "поднимаю $IMAGE"
docker run -d --name "$CONTAINER" -e POSTGRES_PASSWORD=postgres "$IMAGE" >/dev/null

# pg_isready отвечает утвердительно уже во время initdb, когда сервер поднят
# временно и только на unix-сокете, — поэтому ждём успешного запроса,
# а не готовности сокета.
ready=0
for _ in $(seq 1 90); do
    if docker exec -i "$CONTAINER" psql -U postgres -c "SELECT 1" >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 1
done
if [ "$ready" != "1" ]; then
    echo "PostgreSQL не поднялся"
    docker logs "$CONTAINER" | tail -20
    exit 1
fi

docker exec -i "$CONTAINER" psql -U postgres -q -c "CREATE DATABASE wish"
for service in $SERVICES; do
    docker exec -i "$CONTAINER" psql -U postgres -d wish -q -c "CREATE SCHEMA $service"
done

# Порядок берётся из числового префикса имени файла: сортировка по полному
# пути ставит версию 10 перед версией 1.
migrations() {
    ls "services/cmd/$1/migrations"/*."$2".sql | xargs -n1 basename | sort -t_ -k1 "$3"
}

run() {
    docker exec -i "$CONTAINER" psql -U postgres -d wish -v ON_ERROR_STOP=1 -q \
        -c "SET search_path TO $1" -f - < "services/cmd/$1/migrations/$2"
}

for service in $SERVICES; do
    for file in $(migrations "$service" up -n); do
        echo "up   $service/$file"
        run "$service" "$file"
    done
done

for service in $SERVICES; do
    for file in $(migrations "$service" down -rn); do
        echo "down $service/$file"
        run "$service" "$file"
    done
done

left=$(docker exec -i "$CONTAINER" psql -U postgres -d wish -t -A \
    -c "SELECT count(*) FROM information_schema.tables WHERE table_schema IN ('wallet','credit','account','users')")
if [ "$left" != "0" ]; then
    echo "после отката осталось таблиц: $left"
    exit 1
fi
echo "миграции проходят вперёд и назад, после отката таблиц не осталось"
