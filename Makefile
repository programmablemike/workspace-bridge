.PHONY: build test check
build:
	go build -o bin/workspace-bridge ./cmd/workspace-bridge
test:
	go test -race ./...
check:
	go vet ./...
	@test -z "$$(gofmt -l cmd internal)"
