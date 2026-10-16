IMAGE := novellia-fhir-api
PORT  ?= 8080
DATA  ?= data/fhir-resources.jsonl
# The SQLite file, rebuilt on every start. Open it in any SQLite viewer.
DB    := db/novellia.db

.PHONY: run test build docker docker-build docker-run demo testdata scenarios

## run: start the API locally (needs Go 1.26+)
run:
	DATA_FILE=$(DATA) DB_PATH=$(DB) PORT=$(PORT) go run ./cmd/server

## test: run all tests
test:
	go test ./...

## build: build the binary into ./server
build:
	CGO_ENABLED=0 go build -o server ./cmd/server

## docker: build the image and run it (the one command most people need)
docker: docker-build docker-run

docker-build:
	docker build -t $(IMAGE) .

# The db/ folder is shared with the container, so the database file it builds
# shows up in the project while the app runs.
docker-run:
	mkdir -p db
	docker run --rm -p $(PORT):8080 -v "$(CURDIR)/db:/app/db" $(IMAGE)

## demo: call every endpoint with cURL (start the API first)
demo:
	./scripts/demo.sh http://localhost:$(PORT)

## testdata: rebuild testdata/messy.jsonl
testdata:
	python3 testdata/generate_messy.py

## scenarios: rebuild testdata/scenarios/*.jsonl, then try one with: make run DATA=testdata/scenarios/07-patient-links.jsonl
scenarios:
	python3 testdata/generate_scenarios.py
