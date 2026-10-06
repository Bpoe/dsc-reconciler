.PHONY: build test vet fmt-check check race

build:
	go build ./...
	go build -o bin/dscd ./cmd/dscd

test:
	go test ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

check: build test vet fmt-check

race:
	go test -race ./...
