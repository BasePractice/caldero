#!/bin/sh

export PATH=~/go/bin:"$PATH"
#go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#go install google.golang.org/grpc/cmd/protoc-gen-go@latest
protoc --go_out=. --go_opt=paths=import --go-grpc_out=. --go-grpc_opt=paths=import middleware/wallet.proto
go build -o .bin/wallet wish/services/cmd/wallet
go build -o .bin/credit wish/services/cmd/credit
go build -o .bin/account wish/services/cmd/account
go build -o .bin/users wish/services/cmd/users