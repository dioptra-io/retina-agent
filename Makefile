.PHONY: build proper test run clean help

help:
	@echo "Valid targets:"
	@echo "  build     - Build retina-agent binary"
	@echo "  proper    - Format code and run linters"
	@echo "  test      - Run tests"
	@echo "  run       - Build and run retina-agent"
	@echo "  clean     - Remove built binaries"

build: proper
	go build -o retina-agent ./cmd/retina-agent

proper:
	find . -name '*.go' | sort | xargs wc -l
	gofmt -s -w $(shell go list -f '{{.Dir}}' ./...)
	@if command -v goimports >/dev/null 2>&1; then \
		echo goimports -w $(shell go list -f '{{.Dir}}' ./...); \
		goimports -w $(shell go list -f '{{.Dir}}' ./...); \
	fi
	golangci-lint run

test:
	go test -v -race -cover ./...

run: build
	./retina-agent

clean:
	rm -f retina-agent