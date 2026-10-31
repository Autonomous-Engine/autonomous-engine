.PHONY: build test lint clean docker-build docker-up docker-down verifier-eval

# Build all Go services
build:
	@echo "Building all services..."
	@for dir in cmd/*/; do \
		service=$$(basename $$dir); \
		echo "  Building $$service..."; \
		CGO_ENABLED=0 go build -o bin/$$service ./$$dir; \
	done

# Run all tests
test:
	go test ./... -v -count=1

# Seeded-defect evaluation of the independent verifier (M3-07; see
# cmd/verifier-eval/README.md). Dry run unless VERIFIER_EVAL_PROVIDER /
# VERIFIER_EVAL_MODEL (or ANTHROPIC_API_KEY + VERIFIER_EVAL_MODEL) are set.
verifier-eval:
	go run ./cmd/verifier-eval -out verifier-eval.md

# Run tests with coverage
test-coverage:
	go test ./... -coverprofile=coverage.out -count=1
	go tool cover -func=coverage.out

# Lint (requires golangci-lint)
lint:
	golangci-lint run ./...

# Format
fmt:
	go fmt ./...
	goimports -w .

# Tidy dependencies
tidy:
	go mod tidy

# Clean build artifacts
clean:
	rm -rf bin/ coverage.out

# Docker
docker-build:
	docker-compose build

docker-up:
	docker-compose up -d

docker-down:
	docker-compose down

# Build dashboard frontend
dashboard-build:
	cd dashboard && npm install && npm run build

# Build portal frontend
portal-build:
	cd portal && npm install && npm run build

# Full build (Go + frontends + Docker)
all: tidy build dashboard-build portal-build docker-build
