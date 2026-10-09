.PHONY: build test vet fmt-check verify check race

build:
	go build ./...
	go build -o bin/dscd ./cmd/dscd

test:
	go test ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

verify:
	go mod verify

check: verify build test vet fmt-check

race:
	go test -race ./...
