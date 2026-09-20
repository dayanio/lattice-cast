.PHONY: test test-short lint build verify

test:
	@if find . -name '*.go' -not -path './android/*' | grep -q .; then go test ./... -count=1; else echo "no go sources yet"; fi

test-short:
	@if find . -name '*.go' -not -path './android/*' | grep -q .; then go test ./... -count=1 -short; else echo "no go sources yet"; fi

lint:
	golangci-lint run

build:
	go build ./...

verify: lint test build
