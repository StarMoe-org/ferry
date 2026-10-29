VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARY  ?= bin/ferry

.PHONY: all web build test vet demo clean docker

all: build

web:
	cd web && npm ci --no-audit --no-fund && npm run build

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/ferry

vet:
	go vet ./...

test:
	go test -race ./...

demo: build
	./$(BINARY) -demo

docker:
	docker build --build-arg VERSION=$(VERSION) -t ferry:$(VERSION) .

clean:
	rm -rf bin ferry
