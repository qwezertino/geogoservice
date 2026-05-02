.PHONY: build up down scale tidy lint test setup ps logs

## setup: prepare data directories with correct permissions (run once after git clone)
setup:
	mkdir -p data/postgres data/minio
	chmod 777 data/postgres data/minio

## build: build all Docker images
build:
	docker compose build

## up: start the full stack with 3 replicas (default)
up: setup
	docker compose up -d --build --scale gogeoapp=3

## scale n=N: change the number of running app replicas on-the-fly
##   e.g.  make scale n=5
scale:
	docker compose up -d --scale gogeoapp=$(n) --no-recreate

## down: tear down all containers and networks (data volumes preserved)
down:
	docker compose down

## logs: tail logs from all services
logs:
	docker compose logs -f

## logs: tail logs from all services
ps:
	docker compose ps

## tidy: run go mod tidy inside a temporary container (requires GDAL for CGO)
tidy:
	docker compose run --rm --no-deps gogeoapp go mod tidy

## test: run Go unit tests inside a temporary container
test:
	docker compose run --rm --no-deps gogeoapp go test ./...

## lint: run golangci-lint (requires golangci-lint in PATH)
lint:
	cd app && golangci-lint run ./...
