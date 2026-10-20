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
	return newTestServerWithCell(t, file, 5)
}

func newTestServerWithCell(t *testing.T, file string, minCellSize int) http.Handler {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema, _ := validate.NewSchema(loader.SupportedTypes())
	sum, err := loader.LoadFile(context.Background(), db, file, schema, loader.Config{
		MaxBinaryBytes: 100, MaxRecordBytes: 2000,
		Now: func() time.Time { return time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(db, sum, Options{PatientIDSecret: "test-secret", MinCellSize: minCellSize})
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
		Count    int              `json:"countWithoutDuplicates"`
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
		Count int `json:"countWithoutDuplicates"`
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

func TestUnsupportedTypeKeptRaw(t *testing.T) {
	// Immunization has no clean table: it is kept raw, for the data team only.
	h := newTestServer(t, "../../testdata/messy.jsonl")
	if body := get(t, h, "/fhir/Immunization/imm1", http.StatusOK); !strings.Contains(body, `"imm1"`) {
		t.Errorf("Immunization not returned: %s", body)
	}
}

func TestCohortExclusionAndProcedure(t *testing.T) {
	h := newTestServer(t, "../../data/fhir-resources.jsonl")
	count := func(path string) int {
		var resp struct {
			Count int `json:"countWithoutDuplicates"`
		}
		json.Unmarshal([]byte(get(t, h, path, http.StatusOK)), &resp)
		return resp.Count
	}
	diabetes := count("/research/cohort?condition=diabetes")
	if diabetes == 0 {
		t.Fatal("expected patients with diabetes")
	}
	// Noah has diabetes and high cholesterol; excluding cholesterol must drop him.
	if got := count("/research/cohort?condition=diabetes&excludeCondition=hypercholesterolemia"); got != diabetes-1 {
		t.Errorf("exclusion: got %d, want %d", got, diabetes-1)
	}
	if got := count("/research/cohort?procedure=colonoscopy"); got != 1 {
		t.Errorf("procedure filter: got %d, want 1", got)
	}
	if got := count("/research/cohort?excludeProcedure=colonoscopy"); got != 15 {
		t.Errorf("procedure exclusion: got %d, want 15", got)
	}
}

func TestCohortCountsPossibleDuplicates(t *testing.T) {
	h := newTestServer(t, "../../testdata/scenarios/09-two-clinics.jsonl")
	body := get(t, h, "/research/cohort", http.StatusOK)
	var resp struct {
		Count    int              `json:"countWithoutDuplicates"`
		Dups     int              `json:"possibleDuplicatePatients"`
		All      int              `json:"countIncludingPossibleDuplicates"`
		Patients []map[string]any `json:"patients"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 || resp.Dups != 1 || resp.All != 2 {
		t.Errorf("want count 1 (Rosa once), 2 including possible duplicates: %s", body)
	}
	if _, ok := resp.Patients[0]["possibleDuplicateOf"]; !ok {
		t.Errorf("expected possibleDuplicateOf on each patient: %s", body)
	}
	if strings.Contains(body, "clinic-a-1") || strings.Contains(body, "Rosa") {
		t.Error("duplicate info must not leak patient ids or names")
	}
}

func TestDatasetProfile(t *testing.T) {
	h := newTestServer(t, "../../data/fhir-resources.jsonl")
	body := get(t, h, "/research/dataset-profile", http.StatusOK)
	var resp struct {
		Patients   int                       `json:"patientsWithoutDuplicates"`
		History    map[string]any            `json:"history"`
		Population map[string]map[string]any `json:"population"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Patients != 16 {
		t.Errorf("patients = %d", resp.Patients)
	}
	if resp.History["yearsOfHistory"] == nil {
		t.Errorf("history missing: %s", body)
	}
	// 16 patients split into age bands: small bands must be hidden.
	hidden := false
	for _, v := range resp.Population["ageBands"] {
		if v == "<5" {
			hidden = true
		}
	}
	if !hidden {
		t.Errorf("expected small age bands shown as <5: %v", resp.Population["ageBands"])
	}
	if strings.Contains(body, "noah") || strings.Contains(body, "Wyle") {
		t.Error("profile must not contain patient details")
	}
}

func TestCountsDoNotDoubleCountDuplicatePatients(t *testing.T) {
	// Rosa Parks appears as clinic-a-1 and clinic-b-77, both with diabetes.
	h := newTestServerWithCell(t, "../../testdata/scenarios/09-two-clinics.jsonl", 1)
	body := get(t, h, "/research/cohort?condition=diabetes", http.StatusOK)
	var resp struct {
		Without  int `json:"countWithoutDuplicates"`
		Incl     int `json:"countIncludingPossibleDuplicates"`
		Possible int `json:"possibleDuplicatePatients"`
	}
	json.Unmarshal([]byte(body), &resp)
	if resp.Without != 1 || resp.Incl != 2 || resp.Possible != 1 {
		t.Errorf("diabetes should count Rosa once: %s", body)
	}
}

func TestDatasetProfileCountsPeopleOnce(t *testing.T) {
	h := newTestServer(t, "../../testdata/scenarios/09-two-clinics.jsonl")
	body := get(t, h, "/research/dataset-profile", http.StatusOK)
	var resp struct {
		Patients int `json:"patientsWithoutDuplicates"`
		All      int `json:"patientsIncludingPossibleDuplicates"`
	}
	json.Unmarshal([]byte(body), &resp)
	if resp.Patients != 1 || resp.All != 2 {
		t.Errorf("want 1 patient, 2 including possible duplicates: %s", body)
	}
}

func TestPossibleDuplicatesCountedOnce(t *testing.T) {
	h := newTestServerWithCell(t, "../../testdata/scenarios/13-identifiers.jsonl", 1)

	// Possible duplicates are never combined: each chart shows only its own records.
	if body := get(t, h, "/patients/id-a1/conditions", http.StatusOK); strings.Contains(body, "id-a2-co") {
		t.Errorf("id-a1's chart must not show id-a2's records: %s", body)
	}

	// Research: 11 patients. id-a1/id-a2 and id-h1/id-h2 share a name and birth
	// date, so each pair counts once without possible duplicates.
	body := get(t, h, "/research/cohort", http.StatusOK)
	var resp struct {
		Without  int `json:"countWithoutDuplicates"`
		Incl     int `json:"countIncludingPossibleDuplicates"`
		Possible int `json:"possibleDuplicatePatients"`
	}
	json.Unmarshal([]byte(body), &resp)
	if resp.Incl != 11 || resp.Possible != 2 || resp.Without != 9 {
		t.Errorf("want 11 patients, 2 possible duplicates, 9 without: %+v", resp)
	}
}

func TestSSNNeverReturned(t *testing.T) {
	h := newTestServerWithCell(t, "../../testdata/scenarios/13-identifiers.jsonl", 1)
	for _, path := range []string{
		"/fhir/Patient/id-a1",
		"/patients/id-a1",
		"/data-quality/issues?includeRaw=true",
		"/research/cohort",
	} {
		body := get(t, h, path, http.StatusOK)
		for _, ssn := range []string{"234-56-7890", "234567890", "345-67-8901"} {
			if strings.Contains(body, ssn) {
				t.Errorf("%s leaks an SSN", path)
			}
		}
	}
	if body := get(t, h, "/fhir/Patient/id-a1", http.StatusOK); !strings.Contains(body, "[redacted]") ||
		!strings.Contains(body, "MRN-id-a1") {
		t.Errorf("SSN should be masked and other identifiers kept: %s", body)
	}
}

func TestComponentSearchableByOwnCode(t *testing.T) {
	// Systolic blood pressure is stored as a component, but observation_values
	// lets research find it by its own lab test code.
	h := newTestServerWithCell(t, "../../data/fhir-resources.jsonl", 1)
	body := get(t, h, "/research/cohort?lab=8480-6", http.StatusOK)
	if !strings.Contains(body, `"mmHg"`) {
		t.Errorf("systolic should be found by its own lab test code: %s", body)
	}
}
