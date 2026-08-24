#!/usr/bin/env bash
# Проверяет файлы стенда и доставки: разбираются ли они и не разошёлся ли
# состав стека окружения с кодом.
#
# Стек окружения описан отдельно от локального стенда, и это плата
# за отсутствие в нём сборки, наблюдаемости и открытых портов базы.
# Расплачиваться молчаливым расхождением нельзя: новый сервис, забытый
# в deploy/compose.yml, обнаружился бы только на сервере — тем, что его
# там нет.
set -euo pipefail

COMPOSE="deploy/compose.yml"
DEPLOY="deploy/deploy.sh"
failed=0

fail() {
    echo "  ✗ $1" >&2
    failed=1
}

echo "== состав сервисов"
# Список сервисов — из Makefile: там он и есть источник правды о том,
# что собирается и публикуется.
services=$(awk -F':=' '/^SERVICES/ {print $2}' Makefile)
for service in $services; do
    if grep -qE "^  $service:" "$COMPOSE"; then
        printf '  %-10s есть\n' "$service"
    else
        fail "$service собирается и публикуется, но отсутствует в $COMPOSE"
    fi
done

echo "== шаг миграций"
# Схема есть у того, у кого есть каталог миграций. Пропущенный сервис
# означал бы, что его схему никто не применяет: сервис поднимется
# и остановится сам, но узнать об этом лучше здесь.
migrated=$(sed -n 's/^MIGRATED="\${MIGRATED:-\(.*\)}"$/\1/p' "$DEPLOY")
[ -n "$migrated" ] || fail "в $DEPLOY не найден список сервисов с миграциями"
for dir in services/cmd/*/migrations; do
    service=$(basename "$(dirname "$dir")")
    case " $migrated " in
    *" $service "*) printf '  %-10s есть\n' "$service" ;;
    *) fail "у $service есть миграции, но он не перечислен в MIGRATED ($DEPLOY)" ;;
    esac
done
for service in $migrated; do
    [ -d "services/cmd/$service/migrations" ] ||
        fail "$service перечислен в MIGRATED, но миграций у него нет"
done

echo "== разбор compose"
# Оба файла разбираются docker compose. Значения подставляются пустышками:
# проверяется форма файла, а не секреты.
#
# Локальный стенд проверяется здесь же не для полноты: слипшиеся строки
# однажды сделали docker-compose.yml неразбираемым, и это не обнаружил
# никто — стенд просто перестал подниматься, а CI его не трогает.
for file in docker-compose.yml "$COMPOSE"; do
    if REGISTRY=registry.invalid TAG=check POSTGRES_PASSWORD=check \
        INFLUXDB_PASSWORD=check OAUTH2_GLOBAL_SECRET=check KEY_MASTER_KEY=check \
        OAUTH2_ISSUER=http://check PUBLIC_BASE_URL=http://check WEB_API_BASE=http://check \
        docker compose -f "$file" config -q; then
        printf '  %-20s разбирается\n' "$file"
    else
        fail "$file не разбирается"
    fi
done

[ "$failed" = 0 ] || {
    echo "файлы доставки разошлись с кодом" >&2
    exit 1
}
echo "файлы доставки согласованы с кодом"
