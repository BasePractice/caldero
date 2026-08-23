#!/usr/bin/env bash
# Ждёт настоящей готовности PostgreSQL в контейнере.
#
# Признак — вторая строка о готовности в логах: во время initdb сервер
# поднимается временно и только на unix-сокете, поэтому и pg_isready,
# и обычный запрос в этот момент проходят, после чего сервер
# перезапускается и следующая команда падает.
set -euo pipefail

container="${1:?укажите имя контейнера}"
attempts="${2:-90}"

for _ in $(seq 1 "$attempts"); do
    started=$(docker logs "$container" 2>&1 | grep -c "database system is ready to accept connections" || true)
    if [ "$started" -ge 2 ] && docker exec -i "$container" psql -U postgres -c "SELECT 1" >/dev/null 2>&1; then
        exit 0
    fi
    sleep 1
done

echo "PostgreSQL в контейнере $container не поднялся" >&2
docker logs "$container" 2>&1 | tail -20 >&2
exit 1
