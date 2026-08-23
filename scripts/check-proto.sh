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

# Версии protoc и плагинов попадают в шапку сгенерированных файлов
# и различаются между машинами. Сравнивать нужно код, а не то, какой
# компилятор его написал.
normalize() {
    grep -vE '^(//|// *)[[:space:]]*(-)?[[:space:]]*(protoc|protoc-gen-go|protoc-gen-go-grpc)([[:space:]]|$)' "$1"
}

failed=0
while IFS= read -r generated; do
    original="$backup/$generated"
    if [ ! -f "$original" ]; then
        echo "новый файл не закоммичен: $generated" >&2
        failed=1
        continue
    fi
    if ! diff <(normalize "$original") <(normalize "$generated") >/dev/null; then
        echo "расхождение в $generated" >&2
        diff <(normalize "$original") <(normalize "$generated") | head -20 >&2
        failed=1
    fi
done < <(find middleware -name '*.pb.go')

rm -rf middleware
cp -R "$backup/middleware" .

if [ "$failed" != "0" ]; then
    echo "сгенерированный код не соответствует .proto — выполните make proto" >&2
    exit 1
fi
echo "сгенерированный код соответствует .proto"
