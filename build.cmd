@echo off

set PATH=E:\Programs\protobuf\bin;%PATH%

protoc --go_out=. --go_opt=paths=import --go-grpc_out=. --go-grpc_opt=paths=import middleware/wallet.proto
go build -o .bin/wallet.exe wish/services/cmd/wallet