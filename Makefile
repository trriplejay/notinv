BINARY  := notinv
BIN_DIR := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint fmt run clean ci vet fmt-check

build:
	CGO_ENABLED=1 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/notinv

test:
	go test -race ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found; install: https://golangci-lint.run/welcome/install/"; exit 1; }
	golangci-lint run ./...

vet:
	go vet ./...

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

ci: fmt-check vet test lint

fmt:
	gofmt -w .

run: build
	./$(BIN_DIR)/$(BINARY)

clean:
	rm -rf $(BIN_DIR)
