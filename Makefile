SHELL := /bin/bash
BINARY := external-dns-porkbun-webhook
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
GO_BUILD := CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)"
CHART := charts/$(BINARY)
HELM_TEST_ARGS := --set legacyStandalone.acceptRisk=true --set porkbun.domain=example.com --set porkbun.existingSecret.name=porkbun-creds
EXTERNAL_DNS_CHART_VERSION := 1.21.1
EXTERNAL_DNS_CHART_URL := https://github.com/kubernetes-sigs/external-dns/releases/download/external-dns-helm-chart-$(EXTERNAL_DNS_CHART_VERSION)/external-dns-$(EXTERNAL_DNS_CHART_VERSION).tgz

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
	@command -v golangci-lint >/dev/null || { echo "golangci-lint is required (https://golangci-lint.run/welcome/install/)" >&2; exit 1; }
	golangci-lint run ./...

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
	helm lint --strict $(CHART) $(HELM_TEST_ARGS)

.PHONY: helm-template
helm-template:
	helm template $(BINARY) $(CHART) --namespace external-dns $(HELM_TEST_ARGS) >/dev/null

.PHONY: helm-template-canonical
helm-template-canonical:
	helm template external-dns $(EXTERNAL_DNS_CHART_URL) \
		--namespace external-dns \
		--values docs/external-dns-values.yaml >/dev/null

.PHONY: helm-check
helm-check: helm-lint helm-template helm-template-canonical

.PHONY: clean
clean:
	rm -rf bin/ coverage.out coverage.html

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
