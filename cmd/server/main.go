// Command server loads the FHIR data file into SQLite and serves the API.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/lwatanabe808/novellia-fhir-api/internal/api"
	"github.com/lwatanabe808/novellia-fhir-api/internal/loader"
	"github.com/lwatanabe808/novellia-fhir-api/internal/store"
	"github.com/lwatanabe808/novellia-fhir-api/internal/validate"
)

func main() {
	dataFile := env("DATA_FILE", "data/fhir-resources.jsonl")
	dbPath := env("DB_PATH", "db/novellia.db")
	port := env("PORT", "8080")
	maxBinaryMB := envInt("MAX_BINARY_MB", 5)
	maxRecordMB := envInt("MAX_RECORD_MB", 10)
	minCellSize := envInt("MIN_CELL_SIZE", 5)
	secret := env("RESEARCH_ID_SECRET", "dev-only-secret-change-me")
	identifierSecret := env("IDENTIFIER_SECRET", "dev-only-identifier-secret-change-me")
	if identifierSecret == "dev-only-identifier-secret-change-me" {
		log.Println("warning: IDENTIFIER_SECRET is not set, using the development default")
	}
	if secret == "dev-only-secret-change-me" {
		log.Println("warning: RESEARCH_ID_SECRET is not set, using the development default")
	}

	schema, err := validate.NewSchema()
	if err != nil {
		log.Fatalf("load FHIR schema: %v", err)
	}
	ranges, err := validate.LoadRanges(os.Getenv("PLAUSIBLE_RANGES_FILE"))
	if err != nil {
		log.Fatalf("load plausible ranges: %v", err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	summary, err := loader.LoadFile(context.Background(), db, dataFile, schema, loader.Config{
		MaxBinaryBytes:   maxBinaryMB << 20,
		MaxRecordBytes:   maxRecordMB << 20,
		Ranges:           ranges,
		IdentifierSecret: []byte(identifierSecret),
	})
	if err != nil {
		log.Fatalf("load data: %v", err)
	}
	log.Printf("database: %s", dbPath)
	log.Printf("loaded %s: %d lines, %d loaded, %d raw only, %d failed, %d issues (see GET /data-quality/summary) in %dms",
		dataFile, summary.Lines, summary.Loaded, summary.RawOnly, summary.Failed, summary.IssueCount, summary.DurationMS)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           api.NewServer(db, summary, api.Options{ResearchIDSecret: secret, MinCellSize: minCellSize}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("listening on http://localhost:%s", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	// Stop cleanly on Ctrl+C or when Docker stops the container.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Fatalf("%s must be a positive whole number, got %q", key, v)
	}
	return n
}
