.PHONY: build build-darwin-arm64 build-linux-amd64 test
build: build-darwin-arm64 build-linux-amd64

build-darwin-arm64:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o bin/ttunnel-darwin-arm64 .

build-linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/ttunnel-linux-amd64 .

test:
	go test -race ./...
