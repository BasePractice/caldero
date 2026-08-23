# Один параметризованный образ вместо четырёх копий: раньше изменение
# в любом сервисе инвалидировало слои всех четырёх.
FROM golang:1.24-alpine3.21 AS builder
LABEL authors="Pastor"

ARG SERVICE
WORKDIR /src

# Alpine не содержит базу часовых поясов, а корневые сертификаты нужны
# итоговому образу из scratch.
RUN apk add --no-cache ca-certificates tzdata

# Зависимости отдельным слоем: он переиспользуется, пока go.mod и go.sum
# не изменились. go mod tidy здесь не место — он выполнялся до COPY . .,
# вычищал go.mod до пустого состояния и делал сборку зависящей от сети.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /service wish/services/cmd/${SERVICE}

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
