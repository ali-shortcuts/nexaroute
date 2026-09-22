.PHONY: fmt test vet race verify run build linux

fmt:
	gofmt -w cmd internal

test:
	go test ./...

vet:
	go vet ./...

race:
	go test -race ./...

verify:
	./scripts/verify.sh

build:
	mkdir -p bin && go build -trimpath -o bin/ulg ./cmd/gateway

linux:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/ulg-linux-amd64 ./cmd/gateway
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o bin/ulg-linux-arm64 ./cmd/gateway

run:
	./run-local.sh
