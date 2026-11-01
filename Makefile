VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= hammurapi
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test test-integration generate lint image run-api run-worker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/hammurapi ./cmd/hammurapi

generate:
	go generate ./...

test:
	go test ./...

# Needs a running Docker daemon (dockertest starts Postgres).
test-integration:
	go test -tags integration -count=1 ./...

lint:
	go vet ./...
	go vet -tags integration ./...

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

run-api: build
	./bin/hammurapi api

run-worker: build
	./bin/hammurapi worker
