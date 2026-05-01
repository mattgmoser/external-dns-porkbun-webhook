SHELL := /bin/bash
BINARY := external-dns-porkbun-webhook
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
GO_BUILD := CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)"

.PHONY: all
all: lint test build

.PHONY: build
build:
	$(GO_BUILD) -o bin/$(BINARY) ./

.PHONY: build-all
build-all:
	GOOS=linux GOARCH=amd64                $(GO_BUILD) -o bin/$(BINARY)-linux-amd64    ./
	GOOS=linux GOARCH=arm64                $(GO_BUILD) -o bin/$(BINARY)-linux-arm64    ./
	GOOS=linux GOARCH=arm GOARM=7          $(GO_BUILD) -o bin/$(BINARY)-linux-armv7    ./
	GOOS=darwin GOARCH=arm64               $(GO_BUILD) -o bin/$(BINARY)-darwin-arm64   ./

.PHONY: test
test:
	go test -race -cover ./...

.PHONY: test-coverage
test-coverage:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

.PHONY: lint
lint:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)
	@command -v golangci-lint >/dev/null && golangci-lint run ./... || echo "golangci-lint not installed; skipping"

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: docker
docker:
	docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 \
		--build-arg VERSION=$(VERSION) \
		-t ghcr.io/mattgmoser/$(BINARY):$(VERSION) \
		-t ghcr.io/mattgmoser/$(BINARY):latest \
		--push .

.PHONY: helm-lint
helm-lint:
	helm lint charts/$(BINARY)

.PHONY: helm-template
helm-template:
	helm template $(BINARY) charts/$(BINARY)

.PHONY: clean
clean:
	rm -rf bin/ coverage.out coverage.html

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
