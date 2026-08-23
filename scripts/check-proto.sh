#!/usr/bin/env bash
# Проверяет, что закоммиченные *.pb.go соответствуют .proto.
#
# Сгенерированный код хранится в репозитории, чтобы сборка не требовала
# protoc. Плата за это — возможность расхождения: правку .proto легко
# закоммитить, забыв перегенерировать. Проверка снимает эту плату.
set -euo pipefail

if ! command -v protoc >/dev/null 2>&1; then
    echo "protoc не найден в PATH, проверка пропущена" >&2
    exit 0
fi

backup=$(mktemp -d)
trap 'rm -rf "$backup"' EXIT

cp -R middleware "$backup/"
make proto >/dev/null

if ! diff -r "$backup/middleware" middleware >/dev/null 2>&1; then
    echo "сгенерированный код не соответствует .proto — выполните make proto" >&2
    diff -r "$backup/middleware" middleware || true
    rm -rf middleware
    cp -R "$backup/middleware" .
    exit 1
fi
echo "сгенерированный код соответствует .proto"
