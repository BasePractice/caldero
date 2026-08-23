# Пути к инструментам берутся из PATH: в прежних build.sh и build.cmd они были
# захардкожены под конкретные машины.
SERVICES := wallet credit account users
BIN      := .bin
GOFLAGS  := -trimpath
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS  := -X wish/services.Version=$(VERSION) -X wish/services.Revision=$(REVISION)

.DEFAULT_GOAL := help
.PHONY: help build test test-race test-integration lint vet fmt fmt-check tidy-check proto up down logs migrate-status migrate-check vuln bench load-test images clean save-keycloak-config

help: ## Показать список целей
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

build: ## Собрать все сервисы в .bin
	@mkdir -p $(BIN)
	@for s in $(SERVICES); do \
		echo "сборка $$s"; \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/$$s wish/services/cmd/$$s || exit 1; \
	done

test: ## Прогнать тесты
	go test ./...

test-race: ## Прогнать тесты с детектором гонок
	go test -race -cover ./...

test-integration: ## Прогнать интеграционные тесты (нужен docker)
	go test -race -tags=integration ./...

vet: ## go vet
	go vet ./...

fmt: ## Отформатировать код
	gofmt -w .

fmt-check: ## Проверить форматирование, не меняя файлы
	@files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then echo "не отформатировано:"; echo "$$files"; exit 1; fi

tidy-check: ## Проверить, что go.mod и go.sum согласованы
	go mod tidy -diff

lint: ## golangci-lint (если установлен)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint не установлен: https://golangci-lint.run/welcome/install/"; exit 1; }
	golangci-lint run

proto: ## Перегенерировать код из middleware/wallet.proto
	@command -v protoc >/dev/null 2>&1 || { echo "protoc не найден в PATH"; exit 1; }
	@command -v protoc-gen-go >/dev/null 2>&1 || { echo "protoc-gen-go не найден: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest"; exit 1; }
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || { echo "protoc-gen-go-grpc не найден: go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest"; exit 1; }
	protoc --go_out=. --go_opt=paths=import --go-grpc_out=. --go-grpc_opt=paths=import middleware/wallet.proto

bench: ## Прогнать бенчмарки
	go test -run '^$$' -bench . -benchmem ./...

load-test: ## Нагрузочный прогон по сервису кредитов (нужен запущенный сервис)
	docker run --rm -i --add-host host.docker.internal:host-gateway \
		-e BASE_URL="$${BASE_URL:-http://host.docker.internal:51052}" \
		-e OPERATOR_ID="$${OPERATOR_ID:-0f95e97c-0ea4-476f-9146-d015ec22e240}" \
		-v "$(PWD)/scripts/load":/scripts grafana/k6 run /scripts/credit.js

vuln: ## Проверить известные уязвимости
	@command -v govulncheck >/dev/null 2>&1 || go install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck ./...

migrate-check: ## Прогнать миграции up и down на временном PostgreSQL
	@scripts/check-migrations.sh

images: ## Собрать образы всех сервисов
	@for s in $(SERVICES); do \
		echo "образ $$s"; \
		docker build --build-arg SERVICE=$$s --build-arg VERSION=$(VERSION) \
			--build-arg REVISION=$(REVISION) -t wish/$$s:latest . || exit 1; \
	done

up: ## Поднять стенд (режим провайдера — из .env)
	docker compose up -d --build

down: ## Остановить стенд
	docker compose down

logs: ## Логи сервисов
	docker compose logs -f wallet credit account users

migrate-status: ## Показать применённые версии схем
	@for s in $(SERVICES); do \
		echo -n "$$s: "; \
		docker compose exec -T postgres-db psql -U postgres -d wish -t -A \
			-c "SELECT version || CASE WHEN dirty THEN ' (dirty)' ELSE '' END FROM $$s.schema_migrations" 2>/dev/null || echo "схема не создана"; \
	done

save-keycloak-config: ## Выгрузить realm Keycloak в config/keycloak/realms
	docker compose exec keycloak /opt/keycloak/bin/kc.sh export \
		--dir /opt/keycloak/data/import --realm krakend --users realm_file

clean: ## Удалить артефакты сборки
	rm -rf $(BIN)
