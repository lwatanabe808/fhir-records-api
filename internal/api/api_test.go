package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lwatanabe808/novellia-fhir-api/internal/loader"
	"github.com/lwatanabe808/novellia-fhir-api/internal/store"
	"github.com/lwatanabe808/novellia-fhir-api/internal/validate"
)

func newTestServer(t *testing.T, file string) http.Handler {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema, _ := validate.NewSchema()
	ranges, _ := validate.LoadRanges("")
	sum, err := loader.LoadFile(context.Background(), db, file, schema, loader.Config{
		MaxBinaryBytes: 100, MaxRecordBytes: 2000, Ranges: ranges,
		Now: func() time.Time { return time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(db, sum, "test-secret")
}

func get(t *testing.T, h http.Handler, path string, wantStatus int) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != wantStatus {
		t.Fatalf("GET %s: status %d, want %d\n%s", path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec.Body.String()
}

func TestErrors(t *testing.T) {
	h := newTestServer(t, "../../testdata/messy.jsonl")
	for path, want := range map[string]int{
		"/patients?limit=abc":                      http.StatusBadRequest,
		"/patients?gender=robot":                   http.StatusBadRequest,
		"/patients/p1/observations?from=yesterday": http.StatusBadRequest,
		"/research/cohort?labMin=7":                http.StatusBadRequest, // labMin needs lab
		"/research/cohort?minAge=60&maxAge=20":     http.StatusBadRequest,
		"/patients/nobody":                         http.StatusNotFound,
		"/fhir/Observation/nope":                   http.StatusNotFound,
		"/no-such-endpoint":                        http.StatusNotFound,
	} {
		body := get(t, h, path, want)
		if !strings.Contains(body, `"error"`) || !strings.Contains(body, `"code"`) {
			t.Errorf("%s: error body has the wrong shape: %s", path, body)
		}
	}
}

func TestPatientIDAnyCapitalization(t *testing.T) {
	h := newTestServer(t, "../../data/fhir-resources.jsonl")
	body := get(t, h, "/patients/NOAH-WYLE", http.StatusOK)
	if !strings.Contains(body, `"id": "noah-wyle"`) {
		t.Errorf("expected noah-wyle, got %s", body)
	}
}

func TestHiddenObservations(t *testing.T) {
	h := newTestServer(t, "../../testdata/messy.jsonl")
	visible := get(t, h, "/patients/p1/observations", http.StatusOK)
	for _, id := range []string{"o-g2", "o-orig", "o-o2", "o-eie"} {
		if strings.Contains(visible, `"`+id+`"`) {
			t.Errorf("%s should be hidden by default", id)
		}
	}
	for _, id := range []string{"o-g1", "o-amend", "o-implausible"} {
		if !strings.Contains(visible, `"`+id+`"`) {
			t.Errorf("%s should be visible", id)
		}
	}
	all := get(t, h, "/patients/p1/observations?includeHidden=true", http.StatusOK)
	for _, id := range []string{"o-g2", "o-orig", "o-o2", "o-eie"} {
		if !strings.Contains(all, `"`+id+`"`) {
			t.Errorf("%s should appear with includeHidden=true", id)
		}
	}
}

func TestCohortIsDeidentified(t *testing.T) {
	h := newTestServer(t, "../../data/fhir-resources.jsonl")
	body := get(t, h, "/research/cohort?condition=diabetes&lab=a1c&labMin=7", http.StatusOK)
	var resp struct {
		Count    int              `json:"count"`
		Patients []map[string]any `json:"patients"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count == 0 {
		t.Fatal("expected at least one patient with diabetes and A1c over 7")
	}
	for _, leak := range []string{"noah-wyle", "Noah", "Wyle", "birthDate", "1971-06-04"} {
		if strings.Contains(body, leak) {
			t.Errorf("cohort response leaks %q", leak)
		}
	}
	if id, _ := resp.Patients[0]["researchId"].(string); !strings.HasPrefix(id, "r-") {
		t.Errorf("research id %q", id)
	}
}

func TestCohortSkipsRefutedDiagnosis(t *testing.T) {
	h := newTestServer(t, "../../testdata/messy.jsonl")
	body := get(t, h, "/research/cohort?condition=44054006", http.StatusOK)
	var resp struct {
		Count int `json:"count"`
	}
	json.Unmarshal([]byte(body), &resp)
	if resp.Count != 1 { // p1 has diabetes; p2's diagnosis was refuted
		t.Errorf("count = %d, want 1\n%s", resp.Count, body)
	}
}

func TestNotesShowDecodedTextAndReasons(t *testing.T) {
	h := newTestServer(t, "../../testdata/messy.jsonl")
	body := get(t, h, "/patients/p1/notes", http.StatusOK)
	for _, want := range []string{"Inline note text.", "Patient is stable.", "is not in the data", "not text"} {
		if !strings.Contains(body, want) {
			t.Errorf("notes response missing %q", want)
		}
	}
}

func TestUnsupportedTypeReachable(t *testing.T) {
	h := newTestServer(t, "../../testdata/messy.jsonl")
	body := get(t, h, "/patients/p1/records?type=Immunization", http.StatusOK)
	if !strings.Contains(body, `"imm1"`) {
		t.Errorf("Immunization not returned: %s", body)
	}
}

func TestSingleRecord(t *testing.T) {
	h := newTestServer(t, "../../data/fhir-resources.jsonl")
	// A hidden record can still be fetched by id, with the reason it's hidden.
	body := get(t, h, "/patients/tracy-ifeachor/observations/obs-ti-003", http.StatusOK)
	if !strings.Contains(body, "replaced by obs-ti-005") {
		t.Errorf("expected the hidden reason, got %s", body)
	}
	// A record that belongs to another patient is not found.
	get(t, h, "/patients/noah-wyle/observations/obs-ti-005", http.StatusNotFound)
	get(t, h, "/patients/noah-wyle/conditions/nope", http.StatusNotFound)
	get(t, h, "/patients/noah-wyle/notes/docref-001", http.StatusOK)
}

func TestSummary(t *testing.T) {
	h := newTestServer(t, "../../data/fhir-resources.jsonl")
	body := get(t, h, "/patients/noah-wyle/summary", http.StatusOK)
	var resp struct {
		ActiveConditions []map[string]any `json:"activeConditions"`
		LatestLabs       []map[string]any `json:"latestLabs"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.ActiveConditions) == 0 {
		t.Error("expected active conditions")
	}
	// Latest A1c is 7.4 (Feb), not the older 7.8 or the hidden 7.9 duplicate.
	for _, lab := range resp.LatestLabs {
		if lab["code"] == "4548-4" && lab["value"] != 7.4 {
			t.Errorf("latest A1c = %v, want 7.4", lab["value"])
		}
	}
}
