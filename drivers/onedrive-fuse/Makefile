VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO ?= go

.PHONY: build test vet install clean

build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o bin/onedrive-fuse ./cmd/onedrive-fuse
	$(GO) build -o bin/mock-graph ./cmd/mock-graph

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

install: build
	install -Dm755 bin/onedrive-fuse $(HOME)/.local/bin/onedrive-fuse
	install -Dm644 contrib/systemd/onedrive-fuse.service $(HOME)/.config/systemd/user/onedrive-fuse.service

clean:
	rm -rf bin
