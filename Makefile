.PHONY: all build web

ver := $(shell git describe --tags --always --dirty)

build:
	go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node cmd/node/main.go

all:
	GOOS=linux GOARCH=amd64 go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node-linux-amd64 cmd/node/main.go
	GOOS=linux GOARCH=arm64 go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node-linux-arm64 cmd/node/main.go
	GOOS=darwin GOARCH=arm64 go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node-mac-arm64 cmd/node/main.go
	GOOS=darwin GOARCH=amd64 go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node-mac-amd64 cmd/node/main.go
	GOOS=windows GOARCH=amd64 go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node-x64.exe cmd/node/main.go

web:
	tinygo build -target=wasm -tags=js -no-debug -o cmd/web/web-payments-frontend/public/web.wasm cmd/web/main.go
	ls -l cmd/web/web-payments-frontend/public/web.wasm
