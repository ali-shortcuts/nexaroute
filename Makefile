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
	mkdir -p bin && go build -trimpath -o bin/nexaroute ./cmd/gateway

linux:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-linux-amd64 ./cmd/gateway
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-linux-arm64 ./cmd/gateway
	(cd bin && sha256sum nexaroute-linux-amd64 nexaroute-linux-arm64 > SHA256SUMS)
	cp bin/SHA256SUMS SHA256SUMS

run:
	./run-local.sh
