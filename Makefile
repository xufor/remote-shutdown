VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
export CGO_ENABLED := 0

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o web-shutdown .

# Cross-compile every release binary into dist/
release:
	rm -rf dist && mkdir dist
	GOOS=linux GOARCH=arm64       go build -trimpath -ldflags "$(LDFLAGS)" -o dist/web-shutdown-linux-arm64 .
	GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/web-shutdown-linux-armv7 .
	GOOS=linux GOARCH=arm GOARM=6 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/web-shutdown-linux-armv6 .
	GOOS=linux GOARCH=amd64       go build -trimpath -ldflags "$(LDFLAGS)" -o dist/web-shutdown-linux-amd64 .
	cd dist && sha256sum web-shutdown-* > SHA256SUMS

install: build
	./web-shutdown install

.PHONY: build release install
