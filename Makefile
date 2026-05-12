BINARY   = fetcher
CMD      = ./cmd/server
BUILD_DIR = ./bin

.PHONY: build run tidy lint test clean docker-build

build:
	go build -o $(BUILD_DIR)/$(BINARY) $(CMD)

run:
	go run $(CMD)/main.go

tidy:
	go mod tidy

lint:
	go vet ./...

test:
	go test -race ./...

clean:
	rm -rf $(BUILD_DIR)

docker-build:
	docker build -t nightowl-fetcher:latest .

## Python integration helpers
.PHONY: client-test
client-test:
	@echo "Testing listing endpoint..."
	curl -s -X POST http://localhost:8080/fetch/listing \
		-H "Content-Type: application/json" \
		-d '{"url":"https://truyencom.com/truyen-tien-hiep/full/"}' | head -5

.PHONY: client-story
client-story:
	@test -n "$(URL)" || (echo "Usage: make client-story URL=https://..."; exit 1)
	curl -s -X POST http://localhost:8080/fetch/story \
		-H "Content-Type: application/json" \
		-d '{"url":"$(URL)"}'
