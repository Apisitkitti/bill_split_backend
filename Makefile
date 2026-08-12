.PHONY: db migrate run test itest tidy check

db:      ## start Postgres
	docker compose up -d db

migrate: ## apply migrations (needs: go install github.com/pressly/goose/v3/cmd/goose@latest)
	goose -dir migrations postgres "$$(grep '^DATABASE_URL=' .env | cut -d= -f2-)" up

run:     ## run the API on :8080
	go run ./cmd/server

test:    ## unit tests
	go test ./...

itest:   ## tests that need a live database
	TEST_DATABASE_URL="$$(grep '^DATABASE_URL=' .env | cut -d= -f2-)" go test ./internal/repo/ -v

check:   ## what a review must pass
	gofmt -l . && go vet ./... && go test ./...

tidy:
	go mod tidy
