FROM golang:1.24-alpine3.21 as builder
LABEL authors="Pastor"

WORKDIR app
COPY go.mod ./
COPY go.sum ./
RUN go mod download
RUN go mod tidy
COPY . .
RUN CGO_ENABLED=0 go build -o /wallet -ldflags '-extldflags "-static"' wish/services/cmd/wallet

FROM scratch
COPY --from=builder /wallet /wallet

EXPOSE 51051

CMD ["/wallet"]