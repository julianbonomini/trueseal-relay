BINARY=trueseal-relay
BUILD_DIR=./cmd/trueseal-relay
IMAGE=trueseal-relay:latest

.PHONY: build test lint clean docker-build docker-up docker-down pubkey address

build:
	go build -o $(BINARY) $(BUILD_DIR)

test:
	go test -race ./...

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

# Print the relay public key (what clients configure). The keypair is
# auto-generated on first start; to supply your own, set TRUESEAL_RELAY_KEYPAIR_HEX.
pubkey:
	docker compose exec relay trueseal-relay pubkey

# Print the Relay Address (sync ADR-0028) for clients: make address HOST=relay.example.com
address:
	docker compose exec relay trueseal-relay address --host $(HOST)
