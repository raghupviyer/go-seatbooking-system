# make burst BASE_URL=https://my-app.example.com [BURST_FLAGS="-users 500"]
BASE_URL ?= http://localhost:8080

.PHONY: burst test
burst:
	go run ./cmd/burst $(BURST_FLAGS) $(BASE_URL)

test:
	go test ./...
