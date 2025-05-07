@echo off

set PATH=E:\Programs\protobuf\bin;%PATH%

protoc --go_out=. --go_opt=paths=import --go-grpc_out=. --go-grpc_opt=paths=import middleware/wallet.proto
go build -o .bin/wallet.exe wish/services/cmd/wallet
go build -o .bin/credit.exe wish/services/cmd/credit
go build -o .bin/users.exe wish/services/cmd/users
go build -o .bin/account.exe wish/services/cmd/account