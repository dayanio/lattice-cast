.PHONY: test lint build verify

test:
	go test ./... -count=1

lint:
	golangci-lint run

build:
	go build ./...

verify: lint test build
