# Simple Makefile for a Go project

# Build the application
all: build test

build:
	@echo "Building..."
	@templ generate
	@go build -o main cmd/api/main.go

# Run the application
run:
	@templ generate
	@go run cmd/api/main.go

# Test the application
test:
	@echo "Testing..."
	@go test ./... -v

# Clean the binary
clean:
	@echo "Cleaning..."
	@rm -f main

# Wipe the local database and rebuild it from migrations - see cmd/dbreset.
# Interactive (asks for confirmation); pass ARGS=-yes to skip that.
db-reset:
	@go run ./cmd/dbreset $(ARGS)

# Populate the local database with sample orders/print items spread across
# every board column - see cmd/dbseed. Safe to re-run.
seed:
	@go run ./cmd/dbseed

# Live Reload
watch:
	@if command -v air > /dev/null; then \
            air; \
            echo "Watching...";\
        else \
            read -p "Go's 'air' is not installed on your machine. Do you want to install it? [Y/n] " choice; \
            if [ "$$choice" != "n" ] && [ "$$choice" != "N" ]; then \
                go install github.com/air-verse/air@latest; \
                air; \
                echo "Watching...";\
            else \
                echo "You chose not to install air. Exiting..."; \
                exit 1; \
            fi; \
        fi

.PHONY: all build run test clean watch db-reset seed
