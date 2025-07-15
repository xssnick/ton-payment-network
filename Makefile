.PHONY: all build web-dev web-prod

ver := $(shell git describe --tags --always --dirty)

build:
	go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node cmd/node/main.go

all:
	GOOS=linux GOARCH=amd64 go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node-linux-amd64 cmd/node/main.go
	GOOS=linux GOARCH=arm64 go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node-linux-arm64 cmd/node/main.go
	GOOS=darwin GOARCH=arm64 go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node-mac-arm64 cmd/node/main.go
	GOOS=darwin GOARCH=amd64 go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node-mac-amd64 cmd/node/main.go
	GOOS=windows GOARCH=amd64 go build -ldflags "-w -s -X main.GitCommit=$(ver)" -o build/payment-node-x64.exe cmd/node/main.go
	tinygo build -target=wasm -tags=js -no-debug -o build/payment-node-web.wasm cmd/web/main.go
	wasm-opt -Oz -o build/payment-node-web.wasm build/payment-node-web.wasm

web-dev:
	tinygo build -target=wasm -tags=js -no-debug -o cmd/web/web-payments-frontend/public/web.wasm cmd/web/main.go
	ls -lh cmd/web/web-payments-frontend/public/web.wasm

web-prod:
	tinygo build -target=wasm -tags=js -no-debug -o cmd/web/web-payments-frontend/public/web.wasm cmd/web/main.go
	ls -l cmd/web/web-payments-frontend/public/web.wasm
	wasm-opt -Oz -o cmd/web/web-payments-frontend/public/web_opt.wasm cmd/web/web-payments-frontend/public/web.wasm
	ls -l cmd/web/web-payments-frontend/public/web_opt.wasm
	wasm-strip cmd/web/web-payments-frontend/public/web_opt.wasm
	ls -l cmd/web/web-payments-frontend/public/web_opt.wasm
	rm -f cmd/web/web-payments-frontend/public/web_opt.wasm.br
	brotli -Z -o cmd/web/web-payments-frontend/public/web_opt.wasm.br cmd/web/web-payments-frontend/public/web_opt.wasm
	ls -l cmd/web/web-payments-frontend/public/web_opt.wasm.br
	ls -lh cmd/web/web-payments-frontend/public/web_opt.wasm.br
