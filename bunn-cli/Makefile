# Makefile for bunn-cli

.PHONY: build test lint fmt cli install-cli clean help

BINARY_NAME := bunn
BUILD_DIR := .
SRC_DIR := ./cmd/bunn

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

build: ## Build the bunn CLI binary
	go build -o $(BUILD_DIR)/$(BINARY_NAME) $(SRC_DIR)

cli: build ## Alias for build

install-cli: build ## Install the bunn CLI binary to GOPATH/bin
	go install $(SRC_DIR)

test: ## Run all tests
	go test -count=1 ./...

race: ## Run tests with race detector
	go test -race -count=1 ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format Go source files
	gofmt -w .

lint: fmt vet ## Run fmt and vet

tidy: ## Tidy go.mod and go.sum
	go mod tidy

verify: ## Verify module dependencies
	go mod verify

clean: ## Remove build artifacts
	rm -f $(BUILD_DIR)/$(BINARY_NAME)
