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
	maxBinaryMB := envInt("MAX_BINARY_MB", 5) // default 5
	// if one line (record) is too large
	maxRecordMB := envInt("MAX_RECORD_MB", 10) // default 10
	// protects privacy in research results: any count under 5 shows as "<5", so a small group can't point to one person
	minCellSize := envInt("MIN_CELL_SIZE", 5) // default 5
	// secret for hashing patient ids into the ids research partners see
	patientIDSecret := secretEnv("PATIENT_ID_SECRET")

	schema, err := validate.NewSchema(loader.SupportedTypes())
	if err != nil {
		log.Fatalf("load FHIR schema: %v", err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	// closes db when main() ends
	defer db.Close()

	// custom function that writes a transaction to save data to db
	summary, err := loader.LoadFile(context.Background(), db, dataFile, schema, loader.Config{
		MaxBinaryBytes: maxBinaryMB * 1_000_000,
		MaxRecordBytes: maxRecordMB * 1_000_000,
	})
	if err != nil {
		log.Fatalf("load data: %v", err)
	}
	log.Printf("database: %s", dbPath)
	log.Printf("loaded %s: %d lines, %d loaded, %d raw only, %d failed, %d issues (see GET /data-quality/summary) in %dms",
		dataFile, summary.Lines, summary.Loaded, summary.RawOnly, summary.Failed, summary.IssueCount, summary.DurationMS)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           api.NewServer(db, summary, api.Options{PatientIDSecret: patientIDSecret, MinCellSize: minCellSize}),
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

// secretEnv reads a secret setting. Without one it uses a development default
// and warns, so the app still runs locally but nobody mistakes it for production.
func secretEnv(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	log.Printf("warning: %s is not set, using a development default", key)
	return "dev-only-" + key + "-change-me"
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
