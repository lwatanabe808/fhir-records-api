// Package api serves the HTTP endpoints.
//
// There are three groups of endpoints, one per kind of user:
//
//	/patients/...      the Novellia app, showing one person their own records
//	/research/...      life science partners (B2B): de-identified cohorts and counts
//	/data-quality/...  Novellia's data team: what was wrong with the data and what we did
//
// Plus /fhir/{type}/{id} for the original record and /health.
package api

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/lwatanabe808/novellia-fhir-api/internal/loader"
)

// Server holds what the handlers need.
type Server struct {
	db              *sql.DB
	summary         loader.Summary
	patientIDSecret []byte // key for research ids, so they can't be reversed into patient ids
	now             func() time.Time

	// minCellSize: research groups smaller than this are shown as "<N".
	minCellSize int
}

// Options are the settings the API needs beyond the database.
type Options struct {
	PatientIDSecret string
	MinCellSize     int
}

// NewServer builds the HTTP handler.
func NewServer(db *sql.DB, summary loader.Summary, opts Options) http.Handler {
	if opts.MinCellSize < 1 {
		opts.MinCellSize = 1
	}
	s := &Server{db: db, summary: summary, patientIDSecret: []byte(opts.PatientIDSecret), now: time.Now, minCellSize: opts.MinCellSize}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.health)

	// Patient app
	mux.HandleFunc("GET /patients", s.listPatients)
	mux.HandleFunc("GET /patients/{id}", s.getPatient)
	mux.HandleFunc("GET /patients/{id}/summary", s.patientSummary)
	mux.HandleFunc("GET /patients/{id}/conditions", s.patientConditions)
	mux.HandleFunc("GET /patients/{id}/conditions/{recordId}", s.patientConditions)
	mux.HandleFunc("GET /patients/{id}/medications", s.patientMedications)
	mux.HandleFunc("GET /patients/{id}/medications/{recordId}", s.patientMedications)
	mux.HandleFunc("GET /patients/{id}/observations", s.patientObservations)
	mux.HandleFunc("GET /patients/{id}/observations/{recordId}", s.patientObservations)
	mux.HandleFunc("GET /patients/{id}/procedures", s.patientProcedures)
	mux.HandleFunc("GET /patients/{id}/procedures/{recordId}", s.patientProcedures)
	mux.HandleFunc("GET /patients/{id}/notes", s.patientNotes)
	mux.HandleFunc("GET /patients/{id}/notes/{recordId}", s.patientNotes)
	mux.HandleFunc("GET /patients/{id}/timeline", s.patientTimeline)
	mux.HandleFunc("GET /patients/{id}/records", s.patientRawRecords)

	// Research partners (B2B)
	mux.HandleFunc("GET /research/cohort", s.cohort)
	mux.HandleFunc("GET /research/conditions", s.conditionCounts)
	mux.HandleFunc("GET /research/dataset-profile", s.datasetProfile)
	mux.HandleFunc("GET /research/labs/{code}", s.labSummary)

	// Data quality
	mux.HandleFunc("GET /data-quality/summary", s.qualitySummary)
	mux.HandleFunc("GET /data-quality/issues", s.listIssues)

	// Original records
	mux.HandleFunc("GET /fhir/{type}/{id}", s.rawRecord)
	mux.HandleFunc("GET /fhir/{type}/{id}/history", s.rawRecordHistory)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no endpoint at "+r.Method+" "+r.URL.Path)
	})
	return logRequests(recoverPanics(mux))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "load": s.summary})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // show "<5" as written, not as "\u003c5"
	_ = enc.Encode(v)
}

// Every error uses the same shape: {"error": {"code": "...", "message": "..."}}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "something went wrong")
}

// badParam is returned by the query helpers when a parameter is invalid.
type badParam struct{ name, message string }

func (e badParam) Error() string { return e.name + ": " + e.message }

func intParam(r *http.Request, name string, def, min, max int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return 0, badParam{name, "must be a whole number from " + strconv.Itoa(min) + " to " + strconv.Itoa(max)}
	}
	return n, nil
}

func floatParam(r *http.Request, name string) (*float64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, badParam{name, "must be a number"}
	}
	return &n, nil
}

func boolParam(r *http.Request, name string) (bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, badParam{name, "must be true or false"}
	}
	return b, nil
}

func dateParam(r *http.Request, name string) (string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return "", nil
	}
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return "", badParam{name, "must be a date like 2025-01-31"}
	}
	return v, nil
}

func enumParam(r *http.Request, name string, allowed ...string) (string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return "", nil
	}
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	return "", badParam{name, "must be one of " + joinQuoted(allowed)}
}

func joinQuoted(list []string) string {
	s := ""
	for i, v := range list {
		if i > 0 {
			s += ", "
		}
		s += strconv.Quote(v)
	}
	return s
}

// paramError writes a 400 for a bad parameter, and reports whether it did.
func paramError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	writeError(w, http.StatusBadRequest, "INVALID_PARAMETER", err.Error())
	return true
}

func nullStr(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

func nullFloat(f sql.NullFloat64) any {
	if f.Valid {
		return f.Float64
	}
	return nil
}

// ---------- middleware ----------

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		// Only the method and path are logged. Query strings can hold health
		// details (a condition name, for example), so they are left out.
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Microsecond))
	})
}

func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic: %v", p)
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "something went wrong")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
