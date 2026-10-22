package loader

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lwatanabe808/novellia-fhir-api/internal/store"
	"github.com/lwatanabe808/novellia-fhir-api/internal/validate"
)

// Each test here loads one file from testdata/scenarios (built by
// testdata/generate_scenarios.py) and checks how the loader handled it.

func scenario(t *testing.T, name string) (*sql.DB, Summary) {
	t.Helper()
	return load(t, "../../testdata/scenarios/"+name)
}

func wantIssue(t *testing.T, db *sql.DB, record, issueType string) {
	t.Helper()
	if got := issueTypes(t, db, record); !slices.Contains(got, issueType) {
		t.Errorf("%s: issues %v, want %s", record, got, issueType)
	}
}

func wantValue(t *testing.T, db *sql.DB, query, want string) {
	t.Helper()
	got := scalar(t, db, query)
	if (want == "NULL" && got.Valid) || (want != "NULL" && got.String != want) {
		t.Errorf("%s\n  got %q (valid=%v), want %q", query, got.String, got.Valid, want)
	}
}

func TestScenarioCleanFile(t *testing.T) {
	db, sum := scenario(t, "01-clean-new-patients.jsonl")
	if sum.Loaded != 11 || sum.IssueCount != 0 {
		t.Fatalf("want 11 loaded and no issues, got %+v", sum)
	}
	wantValue(t, db, `SELECT full_name FROM patients WHERE id = 'pt-100'`, "Maria Elena García")
	wantValue(t, db, `SELECT measured_at FROM observations WHERE id = 'ob-102'`, "2025-06-02T08:00:00Z") // +02:00 stored as UTC
	wantValue(t, db, `SELECT value_text FROM observation_values WHERE is_component = 0 AND observation_id = 'ob-103'`, "Never smoked tobacco")
	wantValue(t, db, `SELECT performed_at FROM procedures WHERE id = 'pr-100'`, "2024-02-10T08:00:00Z")
	wantValue(t, db, `SELECT onset_date FROM conditions WHERE id = 'co-100'`, "2019")
	wantValue(t, db, `SELECT text FROM binaries WHERE id = 'bn-100'`, "Señora García feels well. ✓")
}

func TestScenarioReverseOrder(t *testing.T) {
	db, sum := scenario(t, "02-reverse-order.jsonl")
	if sum.IssueCount != 0 {
		t.Fatalf("links to records later in the file should resolve, got %d issues", sum.IssueCount)
	}
	wantValue(t, db, `SELECT patient_id FROM observations WHERE id = 'ob-200'`, "pt-200")
	wantValue(t, db, `SELECT patient_id FROM document_references WHERE id = 'dr-200'`, "pt-200")
}

func TestScenarioLineEndings(t *testing.T) {
	_, sum := scenario(t, "03-line-endings.jsonl")
	if sum.Loaded != 3 || sum.BlankLines != 2 || sum.IssueCount != 0 {
		t.Fatalf("want 3 loaded, 2 blank, no issues, got %+v", sum)
	}
}

func TestScenarioAllBroken(t *testing.T) {
	db, sum := scenario(t, "04-all-broken.jsonl")
	if sum.Loaded != 0 || sum.Failed != 11 {
		t.Fatalf("want 0 loaded and 11 failed, got %+v", sum)
	}
	wantValue(t, db, `SELECT COUNT(*) FROM issues WHERE issue_type = 'INVALID_JSON'`, "4")
	wantValue(t, db, `SELECT COUNT(*) FROM issues WHERE issue_type = 'MALFORMED_RESOURCE'`, "7")
}

func TestScenarioPrettyPrinted(t *testing.T) {
	db, sum := scenario(t, "05-pretty-printed.jsonl")
	if sum.Loaded != 2 {
		t.Fatalf("the records around the pretty-printed one should load, got %+v", sum)
	}
	wantValue(t, db, `SELECT COUNT(*) FROM observations WHERE id = 'ob-501'`, "1")
}

func TestScenarioUnsupportedTypes(t *testing.T) {
	db, _ := scenario(t, "06-unsupported-types.jsonl")
	for _, id := range []string{"en-600", "al-600", "im-600", "dr-who", "bu-600"} {
		wantIssue(t, db, id, UnsupportedType)
	}
	wantIssue(t, db, "typo-600", NonStandardType)
	// Unsupported records with a patient link (subject or patient) are still found by patient.
	wantValue(t, db, `SELECT COUNT(*) FROM raw_resources WHERE fhir_id IN ('en-600', 'al-600', 'im-600') AND raw_json IS NOT NULL`, "3")
}

func TestScenarioPatientLinks(t *testing.T) {
	db, _ := scenario(t, "07-patient-links.jsonl")
	wantValue(t, db, `SELECT patient_id FROM observations WHERE id = 'ob-700'`, "NULL") // abc and ABC are the same id twice
	wantIssue(t, db, "ob-700", UnknownPatient)
	wantValue(t, db, `SELECT patient_id FROM observations WHERE id = 'ob-701'`, "NULL") // abc or ABC: never guess
	wantIssue(t, db, "ob-701", UnknownPatient)
	wantValue(t, db, `SELECT patient_id FROM observations WHERE id = 'ob-702'`, "pt-700")
	wantIssue(t, db, "ob-702", CaseMismatchReference)
	for _, id := range []string{"ob-703", "ob-704", "ob-705", "ob-706"} {
		wantIssue(t, db, id, InvalidReference)
		wantValue(t, db, `SELECT patient_id FROM observations WHERE id = '`+id+`'`, "NULL")
	}
	if got := issueTypes(t, db, "ob-705"); len(got) != 1 {
		t.Errorf("an empty link should be reported once, got %v", got)
	}
}

func TestScenarioDates(t *testing.T) {
	db, _ := scenario(t, "08-dates.jsonl")
	// Same day with no time: two real readings, not duplicates.
	wantValue(t, db, `SELECT SUM(is_primary) FROM observations WHERE id IN ('ob-802', 'ob-803')`, "2")
	// Same moment in different time zones: duplicates.
	wantIssue(t, db, "ob-805", DuplicateRecord)
	wantValue(t, db, `SELECT is_primary FROM observations WHERE id = 'ob-805'`, "0")
	wantIssue(t, db, "ob-807", InvalidDate) // February 30
	wantValue(t, db, `SELECT measured_at FROM observations WHERE id = 'ob-807'`, "NULL")
	wantValue(t, db, `SELECT measured_at FROM observations WHERE id = 'ob-800'`, "2020")
}

func TestScenarioTwoClinics(t *testing.T) {
	db, _ := scenario(t, "09-two-clinics.jsonl")
	wantIssue(t, db, "clinic-a-1", PossibleDuplicatePatient)
	wantValue(t, db, `SELECT COUNT(*) FROM patients`, "2") // never merged
	wantIssue(t, db, "b-ob-9", DuplicateRecord)
	wantIssue(t, db, "a-ob-2", DuplicateID)
	wantValue(t, db, `SELECT value FROM observation_values WHERE is_component = 0 AND observation_id = 'a-ob-3'`, "6.8") // newer lastUpdated wins over file order
	wantValue(t, db, `SELECT replaced_by FROM observations WHERE id = 'a-ob-4'`, "a-ob-5")
}

func TestScenarioAttachments(t *testing.T) {
	db, _ := scenario(t, "10-attachments.jsonl")
	wantValue(t, db, `SELECT content_type FROM binaries WHERE id = 'bn-fake-text'`, "image/png")
	wantIssue(t, db, "bn-fake-text", ContentTypeConflict)
	wantValue(t, db, `SELECT text FROM binaries WHERE id = 'bn-spaces'`, "Spaces in base64")
	wantValue(t, db, `SELECT text FROM binaries WHERE id = 'dr-1000-inline-3'`, "Inline text, café ☕")
	wantValue(t, db, `SELECT text FROM binaries WHERE id = 'bn-pdf'`, "NULL")
	wantValue(t, db, `SELECT COUNT(*) FROM document_attachments WHERE binary_id = 'bn-shared'`, "2")
	wantValue(t, db, `SELECT COUNT(*) FROM issues WHERE issue_type = 'ORPHAN_BINARY'`, "0")
	wantIssue(t, db, "dr-1002", UnresolvableAttachment)
}

func TestScenarioValues(t *testing.T) {
	db, _ := scenario(t, "11-values.jsonl")
	wantIssue(t, db, "ob-1102", ImpossibleValue)  // negative
	wantIssue(t, db, "ob-1104", InvalidValueType) // true as a value
	wantValue(t, db, `SELECT value FROM observation_values WHERE is_component = 0 AND observation_id = 'ob-1104'`, "NULL")
	wantValue(t, db, `SELECT value FROM observation_values WHERE is_component = 0 AND observation_id = 'ob-1106'`, "170")
	wantValue(t, db, `SELECT value_text FROM observation_values WHERE is_component = 0 AND observation_id = 'ob-1107'`, "normal")
	if got := issueTypes(t, db, "ob-1108"); !slices.Contains(got, MissingValue) { // logged even with a dataAbsentReason
		t.Errorf("ob-1108: want MISSING_VALUE, got %v", got)
	}
	if got := issueTypes(t, db, "ob-1100"); len(got) != 0 { // other units are kept, not judged
		t.Errorf("ob-1100: want no issues, got %v", got)
	}
	// Valid FHIR value types we don't store are logged, not dropped silently.
	wantIssue(t, db, "ob-1111", UnsupportedValueType) // valueRange
	wantIssue(t, db, "ob-1112", UnsupportedValueType) // valueRatio in a component
	// The standard LOINC code is kept even when the clinic's own code is listed first.
	wantValue(t, db, `SELECT lab_test_code FROM observations WHERE id = 'ob-1113'`, "4548-4")
}

// TestScenarioBulk loads 22,000 records built in memory (the same shape as
// testdata/scenarios/12-bulk.jsonl, which is too big to keep in git).
func TestScenarioBulk(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	var buf bytes.Buffer
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&buf, `{"resourceType":"Patient","id":"bulk-%04d","name":[{"family":"P%04d","given":["Bulk"]}],"gender":"female","birthDate":"%d-01-01"}`+"\n", i, i, 1940+i%60)
		for j := 0; j < 10; j++ {
			fmt.Fprintf(&buf, `{"resourceType":"Observation","id":"bulk-%04d-ob-%d","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"4548-4"}]},"subject":{"reference":"Patient/bulk-%04d"},"effectiveDateTime":"2024-%02d-01T08:00:00Z","valueQuantity":{"value":%.1f,"unit":"%%"}}`+"\n",
				i, j, i, j+1, 5+float64((i+j)%40)/10)
		}
	}
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema, _ := validate.NewSchema(SupportedTypes())
	start := time.Now()
	sum, err := Load(context.Background(), db, &buf, schema, Config{MaxBinaryBytes: 5_000_000, MaxRecordBytes: 10_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Loaded != 22000 || sum.IssueCount != 0 {
		t.Fatalf("want 22000 loaded and no issues, got %+v", sum)
	}
	if took := time.Since(start); took > 60*time.Second {
		t.Errorf("loading 22,000 records took %s", took)
	}
}

func TestScenarioIdentifiers(t *testing.T) {
	db, _ := scenario(t, "13-identifiers.jsonl")
	// SSNs are not used for matching. Same name (ignoring case, accents and
	// punctuation) and birth date: a possible duplicate.
	wantIssue(t, db, "id-a1", PossibleDuplicatePatient) // María García / MARIA GARCIA
	wantIssue(t, db, "id-h1", PossibleDuplicatePatient) // Zoë O'Brien / ZOE O BRIEN
	// Same name, different birth date: not flagged.
	if got := issueTypes(t, db, "id-c1"); len(got) != 0 {
		t.Errorf("id-c1: want no issues, got %v", got)
	}
	// The SSN never appears in the clean tables or in issue text.
	for _, q := range []string{
		`SELECT COUNT(*) FROM patients WHERE full_name LIKE '%2345%'`,
		`SELECT COUNT(*) FROM issues WHERE detail LIKE '%2345%' OR detail LIKE '%3456%'`,
	} {
		wantValue(t, db, q, "0")
	}
}

func TestNameKey(t *testing.T) {
	for _, c := range [][2]string{
		{"María García", "maria garcia"},
		{"ZOË  O'Brien", "zoe o brien"},
		{"Mary-Jane Watson", "mary jane watson"},
	} {
		if got := nameKey(c[0]); got != c[1] {
			t.Errorf("nameKey(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestComponentWithSeveralValues(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema, _ := validate.NewSchema(SupportedTypes())
	file := `{"resourceType":"Patient","id":"p1"}
{"resourceType":"Observation","id":"bp","status":"final","subject":{"reference":"Patient/p1"},
 "code":{"coding":[{"system":"http://loinc.org","code":"85354-9"}]},"effectiveDateTime":"2024-01-01T08:00:00Z",
 "component":[{"code":{"coding":[{"system":"http://loinc.org","code":"8480-6"}]},
   "valueQuantity":{"value":120,"unit":"mmHg"},"valueString":"high"}]}`
	file = strings.ReplaceAll(file, "\n ", " ")
	if _, err := Load(context.Background(), db, strings.NewReader(file), schema, Config{MaxBinaryBytes: 5_000_000, MaxRecordBytes: 10_000_000}); err != nil {
		t.Fatal(err)
	}
	wantIssue(t, db, "bp", MultipleValues)
	// The first value in our order (the number) is kept.
	wantValue(t, db, `SELECT value FROM observation_values WHERE observation_id = 'bp' AND lab_test_code = '8480-6'`, "120")
}
