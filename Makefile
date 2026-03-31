.PHONY: build lint fmt tidy test run clean help

help:
	@echo "Valid targets:"
	@echo "  build  - Format, lint, and build retina-agent binary"
	@echo "  lint   - Format code and run linters"
	@echo "  fmt    - Format code"
	@echo "  tidy   - Tidy go modules"
	@echo "  test   - Run tests with race detection"
	@echo "  run    - Build and run retina-agent"
	@echo "  clean  - Remove built binaries"

build: lint
	go build -o retina-agent ./cmd/retina-agent

lint: fmt
	golangci-lint run

fmt:
	go fmt ./...

tidy:
	go mod tidy

test:
	go test -v -race -cover ./...

run: build
	./retina-agent

clean:
	rm -f retina-agent