FROM golang:1.24-alpine3.21 as builder
LABEL authors="Pastor"

WORKDIR app
COPY go.mod ./
COPY go.sum ./
RUN go mod download
RUN go mod tidy
COPY . .
RUN CGO_ENABLED=0 go build -o /credit -ldflags '-extldflags "-static"' wish/services/cmd/credit

FROM scratch
COPY --from=builder /credit /credit

EXPOSE 51052
EXPOSE 8081

CMD ["/credit"]