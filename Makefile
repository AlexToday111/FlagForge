.PHONY: build test test-race lint integration docker-up docker-down demo benchmark
build:
	go build -o bin/flag-server ./cmd/flag-server
	go build -o bin/flagctl ./cmd/flag-cli
	go build -o bin/example-service ./cmd/example-service
	go build -o bin/loadtest ./cmd/loadtest
test:
	go test ./...
test-race:
	go test -race ./...
lint:
	go vet ./...
	golangci-lint run
integration:
	go test -race -tags=integration -timeout=5m -v ./tests/integration
docker-up:
	docker compose up -d --build
docker-down:
	docker compose down
demo:
	bash scripts/demo.sh
benchmark:
	go test -run '^$$' -bench . -benchmem ./pkg/...
