FROM golang:1.24-alpine3.21 as builder
LABEL authors="Pastor"

WORKDIR app
COPY go.mod ./
COPY go.sum ./
RUN go mod download
RUN go mod tidy
COPY . .
RUN CGO_ENABLED=0 go build -o /account -ldflags '-extldflags "-static"' wish/services/cmd/account

FROM scratch
COPY --from=builder /account /account

EXPOSE 51054

CMD ["/account"]