.PHONY: build vet lint test test-race check

build:
	go build ./...

vet:
	go vet ./...

lint:
	golangci-lint run

test:
	go test ./...

# Every goroutine here owns a child process; races are not a style concern.
test-race:
	go test -race ./...

check: build vet lint test-race
