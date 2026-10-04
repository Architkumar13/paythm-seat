.PHONY: up down logs test burst

TEST_DATABASE_URL ?= postgres://seat:seat@localhost:5433/seats?sslmode=disable
BASE_URL ?= http://localhost:8080

up:
	docker compose up --build -d

down:
	docker compose down -v

logs:
	docker compose logs -f app

test:
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test ./...

burst:
	go run ./cmd/burst --base-url $(BASE_URL)
