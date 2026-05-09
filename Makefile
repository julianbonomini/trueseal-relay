BINARY=hush-relay
BUILD_DIR=./cmd/hush-relay

.PHONY: build test lint clean

build:
	go build -o $(BINARY) $(BUILD_DIR)

test:
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY)
