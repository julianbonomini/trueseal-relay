BINARY=hush-relay
BUILD_DIR=./cmd/hush-relay
IMAGE=hush-relay:latest

.PHONY: build test lint clean docker-build docker-up docker-down genkey

build:
	go build -o $(BINARY) $(BUILD_DIR)

test:
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY)

docker-build:
	docker build -t $(IMAGE) .

docker-up:
	docker compose up -d

docker-down:
	docker compose down

# Generate a relay keypair into ./data/keypair.hex
genkey:
	mkdir -p data
	docker compose run --rm relay /hush-relay -genkey -keyout /data/keypair.hex
