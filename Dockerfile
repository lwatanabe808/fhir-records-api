# Build stage: compile a static binary. The SQLite driver is pure Go, so no C compiler is needed.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# Run stage: a minimal image with no shell, running as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/server /app/server
COPY data/fhir-resources.jsonl /app/data/fhir-resources.jsonl

# To use another data file, mount it and point DATA_FILE at it:
#   docker run -p 8080:8080 -v $(pwd)/other.jsonl:/data/other.jsonl -e DATA_FILE=/data/other.jsonl novellia-fhir-api
ENV DATA_FILE=/app/data/fhir-resources.jsonl \
    DB_PATH=:memory: \
    PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/server"]
