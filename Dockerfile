# Один параметризованный образ вместо четырёх копий: раньше изменение
# в любом сервисе инвалидировало слои всех четырёх.
FROM golang:1.25-alpine AS builder
LABEL authors="Pastor"

ARG SERVICE
ARG VERSION=dev
ARG REVISION=""
WORKDIR /src

# Alpine не содержит базу часовых поясов, а корневые сертификаты нужны
# итоговому образу из scratch.
# Версии не закрепляются намеренно: конкретные версии пакетов Alpine
# исчезают из репозитория при обновлении ветки, и сборка ломается на ровном
# месте. Воспроизводимость здесь обеспечивает тег базового образа.
# hadolint ignore=DL3018
RUN apk add --no-cache ca-certificates tzdata

# Зависимости отдельным слоем: он переиспользуется, пока go.mod и go.sum
# не изменились. go mod tidy здесь не место — он выполнялся до COPY . .,
# вычищал go.mod до пустого состояния и делал сборку зависящей от сети.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Интерфейс собирается до основного бинарника: сервис раздачи встраивает
# статику через embed, и app.wasm должен существовать к моменту компиляции.
RUN if [ "${SERVICE}" = "web" ]; then \
        GOOS=js GOARCH=wasm go build -trimpath \
            -o services/cmd/web/static/app.wasm wish/services/cmd/web/app && \
        cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" services/cmd/web/static/wasm_exec.js; \
    fi

RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X wish/services.Version=${VERSION} -X wish/services.Revision=${REVISION}" \
    -o /service wish/services/cmd/${SERVICE}

FROM scratch

# Без сертификатов любой исходящий HTTPS (маркетплейсы, платёжные сервисы,
# JWKS по https) упадёт с x509: certificate signed by unknown authority.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# Без tzdata весь расчёт времени идёт в UTC независимо от TZ.
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

COPY --from=builder /service /service

# nobody: в scratch процесс иначе работает от root.
USER 65534:65534

# Проба живёт в самом бинарнике: в scratch нет ни оболочки, ни curl.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/service", "-healthcheck"]

ENTRYPOINT ["/service"]
