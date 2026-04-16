.PHONY: manual pages-build build

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

manual:
	go run ./cmd/srv-manual docs manual.html

pages-build:
	mkdir -p dist
	go run ./cmd/srv-manual docs dist/index.html
	touch dist/.nojekyll

build:
	go build -ldflags "-X srv/internal/version.Version=$(VERSION)" ./cmd/srv
