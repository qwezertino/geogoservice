.PHONY: build up down scale tidy lint test

## build: build all Docker images
build:
	docker compose build

## up: start the full stack with 3 replicas (default)
up:
	docker compose up -d --scale ndvi-app=3

## scale n=N: change the number of running app replicas on-the-fly
##   e.g.  make scale n=5
scale:
	docker compose up -d --scale ndvi-app=$(n) --no-recreate

## down: tear down all containers and networks (data volumes preserved)
down:
	docker compose down

## logs: tail logs from all services
logs:
	docker compose logs -f

## tidy: run go mod tidy inside a temporary container (requires GDAL for CGO)
tidy:
	docker compose run --rm --no-deps ndvi-app go mod tidy

## test: run Go unit tests inside a temporary container
test:
	docker compose run --rm --no-deps ndvi-app go test ./...

## lint: run golangci-lint (requires golangci-lint in PATH)
lint:
	golangci-lint run ./...
