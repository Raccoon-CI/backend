GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race vet check clean

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ ./cmd/raccoonci-server

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

# Everything that must pass before merging.
check: vet race

clean:
	rm -rf dist
