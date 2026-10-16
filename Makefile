IMAGE := novellia-fhir-api
PORT  ?= 8080
DATA  ?= data/fhir-resources.jsonl

.PHONY: run test build docker docker-build docker-run demo testdata

## run: start the API locally (needs Go 1.26+)
run:
	DATA_FILE=$(DATA) PORT=$(PORT) go run ./cmd/server

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

docker-run:
	docker run --rm -p $(PORT):8080 $(IMAGE)

## demo: call every endpoint with cURL (start the API first)
demo:
	./scripts/demo.sh http://localhost:$(PORT)

## testdata: rebuild testdata/messy.jsonl
testdata:
	python3 testdata/generate_messy.py
