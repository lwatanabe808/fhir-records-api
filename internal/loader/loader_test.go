package loader

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/lwatanabe808/novellia-fhir-api/internal/store"
	"github.com/lwatanabe808/novellia-fhir-api/internal/validate"
)

func load(t *testing.T, path string) (*sql.DB, Summary) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema, err := validate.NewSchema(SupportedTypes())
	if err != nil {
		t.Fatal(err)
	}
	sum, err := LoadFile(context.Background(), db, path, schema, Config{
		MaxBinaryBytes: 100, // small limits so the test file can trigger them
		MaxRecordBytes: 2000,
		Now:            func() time.Time { return time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return db, sum
}

// issueTypes returns the issue types logged for a record id.
func issueTypes(t *testing.T, db *sql.DB, recordID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT issue_type FROM issues WHERE record_id = ? ORDER BY id`, recordID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func issueOnLine(t *testing.T, db *sql.DB, issueType string) int {
	t.Helper()
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM issues WHERE issue_type = ?`, issueType).Scan(&n)
	return n
}

func scalar(t *testing.T, db *sql.DB, query string, args ...any) sql.NullString {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

// TestMessyFile checks every case in the README's validation table, using
// testdata/messy.jsonl, which has one or more planted problems per case.
func TestMessyFile(t *testing.T) {
	db, sum := load(t, "../../testdata/messy.jsonl")

	if sum.BlankLines != 1 {
		t.Errorf("blank lines = %d, want 1", sum.BlankLines)
	}

	// Issues logged for each record.
	cases := []struct {
		record string
		want   string
	}{
		{"cn1", NonStandardType},
		{"imm1", UnsupportedType},
		{"o-status", InvalidStatus},
		{"o-date", InvalidDate},
		{"o-valtype", InvalidValueType},
		{"o-subjstr", InvalidReference},
		{"o-noprefix", InvalidReference},
		{"o-nosubject", NoPatientLink},
		{"o-nameonly", NoPatientLink},
		{"o-nocode", MissingCode},
		{"o-nosystem", MissingCodeSystem},
		{"o-novalue", MissingValue},
		{"o-multi", MultipleValues},
		{"o-nounit", MissingUnit},
		{"o-o2", ImpossibleValue},
		{"o-future", ImpossibleValue},
		{"o-prebirth", ImpossibleValue},
		{"m-empty", EmptyList},
		{"o-case", CaseMismatchReference},
		{"o-unknown", UnknownPatient},
		{"o-dupid", DuplicateID},
		{"o-changed", DuplicateIDChanged},
		{"o-g1", DuplicateRecord},
		{"o-g2", DuplicateRecord},
		{"o-s1", DuplicateRecord},
		{"o-s2", DuplicateRecord},
		{"o-orig", Superseded},
		{"b-bad", InvalidBinary},
		{"b-nocontent", MissingBinaryContentType},
		{"b-badtype", InvalidContentType},
		{"b-png", ContentTypeConflict},
		{"b-orphan", OrphanBinary},
		{"b-big", FileTooLarge},
		{"d1", BinaryBrokenReference},
		{"d1", ContentTypeMismatch},
		{"d1", UnresolvableAttachment},
		{"d1", InvalidAttachmentURL},
		{"d1", MissingBinary},
		{"o-huge", RecordTooLarge},
		{"p1", PossibleDuplicatePatient},
		{"p2", PossibleDuplicatePatient},
	}
	for _, c := range cases {
		if got := issueTypes(t, db, c.record); !slices.Contains(got, c.want) {
			t.Errorf("%s: issues %v, want %s", c.record, got, c.want)
		}
	}
	for _, typ := range []string{InvalidJSON, MalformedResource, MissingID} {
		if issueOnLine(t, db, typ) == 0 {
			t.Errorf("no %s issue logged", typ)
		}
	}
	if n := issueOnLine(t, db, MalformedResource); n != 2 {
		t.Errorf("MALFORMED_RESOURCE count = %d, want 2", n)
	}

	// What ends up in the clean tables.
	checks := []struct {
		name  string
		query string
		want  string // "" means NULL
	}{
		{"invalid status stored as null", `SELECT status FROM observations WHERE id = 'o-status'`, ""},
		{"invalid date stored as null", `SELECT measured_at FROM observations WHERE id = 'o-date'`, ""},
		{"text value stored as null", `SELECT value FROM observation_values WHERE is_component = 0 AND observation_id = 'o-valtype'`, ""},
		{"multiple values keeps valueQuantity", `SELECT value FROM observation_values WHERE is_component = 0 AND observation_id = 'o-multi'`, "7"},
		{"name-only link left unlinked", `SELECT patient_id FROM observations WHERE id = 'o-nameonly'`, ""},
		{"case mismatch linked", `SELECT patient_id FROM observations WHERE id = 'o-case'`, "p1"},
		{"unknown patient unlinked", `SELECT patient_id FROM observations WHERE id = 'o-unknown'`, ""},
		{"changed copy: later version wins", `SELECT value FROM observation_values WHERE is_component = 0 AND observation_id = 'o-changed'`, "6.5"},
		{"identical copy stored once", `SELECT COUNT(*) FROM observations WHERE id = 'o-dupid'`, "1"},
		{"conflict: lowest id is primary", `SELECT is_primary FROM observations WHERE id = 'o-g1'`, "1"},
		{"conflict: other is not primary", `SELECT is_primary FROM observations WHERE id = 'o-g2'`, "0"},
		{"amended replaces original", `SELECT replaced_by FROM observations WHERE id = 'o-orig'`, "o-amend"},
		{"amended stays primary", `SELECT is_primary FROM observations WHERE id = 'o-amend'`, "1"},
		{"impossible flagged", `SELECT is_impossible FROM observations WHERE id = 'o-o2'`, "1"},
		{"blood pressure parts are both saved", `SELECT COUNT(*) FROM observation_values WHERE observation_id = 'o-bp' AND is_component = 1`, "2"},
		{"high but possible value is not flagged", `SELECT is_impossible FROM observations WHERE id = 'o-implausible'`, "0"},
		{"empty dosage stored as null", `SELECT dosage FROM medications WHERE id = 'm-empty'`, ""},
		{"inline attachment decoded", `SELECT text FROM binaries WHERE id = 'd1-inline-1'`, "Inline note text."},
		{"bad base64: no data", `SELECT data FROM binaries WHERE id = 'b-bad'`, ""},
		{"missing type becomes octet-stream", `SELECT content_type FROM binaries WHERE id = 'b-nocontent'`, "application/octet-stream"},
		{"png labeled text: detected type stored", `SELECT content_type FROM binaries WHERE id = 'b-png'`, "image/png"},
		{"too large: no data", `SELECT data FROM binaries WHERE id = 'b-big'`, ""},
		{"broken binary link kept", `SELECT COUNT(*) FROM document_attachments WHERE binary_id = 'b-missing'`, "1"},
		{"web address not linked", `SELECT COUNT(*) FROM document_attachments WHERE document_id = 'd1'`, "8"},
		{"unsupported type reachable by patient", `SELECT patient_id FROM raw_resources WHERE fhir_id = 'imm1'`, "p1"},
		{"non-FHIR type raw only", `SELECT COUNT(*) FROM raw_resources WHERE fhir_id = 'cn1'`, "1"},
		{"invalid JSON kept as text", `SELECT COUNT(*) FROM raw_resources WHERE parsed_ok = 0`, "1"},
		{"duplicate patients never merged", `SELECT COUNT(*) FROM patients`, "2"},
	}
	for _, c := range checks {
		got := scalar(t, db, c.query)
		if (c.want == "" && got.Valid) || (c.want != "" && got.String != c.want) {
			t.Errorf("%s: got %q (valid=%v), want %q", c.name, got.String, got.Valid, c.want)
		}
	}

	// Duplicate groups share a group key.
	if g := scalar(t, db, `SELECT COUNT(DISTINCT group_key) FROM issues WHERE record_id IN ('o-g1', 'o-g2')`); g.String != "1" {
		t.Errorf("o-g1 and o-g2 should share one group key, got %s", g.String)
	}
	// Same values: info. Different values: warning.
	if s := scalar(t, db, `SELECT severity FROM issues WHERE record_id = 'o-s1' AND issue_type = ?`, DuplicateRecord); s.String != Info {
		t.Errorf("same-value duplicate severity = %s, want info", s.String)
	}
	if s := scalar(t, db, `SELECT severity FROM issues WHERE record_id = 'o-g1' AND issue_type = ?`, DuplicateRecord); s.String != Warning {
		t.Errorf("different-value duplicate severity = %s, want warning", s.String)
	}
}

// TestSampleFile checks the planted problems in the file Novellia sent.
func TestSampleFile(t *testing.T) {
	db, sum := load(t, "../../data/fhir-resources.jsonl")
	if sum.Lines != 129 || sum.Failed != 0 {
		t.Fatalf("summary %+v", sum)
	}
	cases := []struct{ record, want string }{
		{"med-nw-003", CaseMismatchReference},
		{"obs-nw-006", UnknownPatient},
		{"obs-kl-bad-001", NoPatientLink},
		{"cond-nw-bad-001", MissingCode},
		{"obs-nw-002", DuplicateRecord},
		{"obs-nw-dup-001", DuplicateRecord},
		{"obs-ti-003", Superseded},
		{"med-fd-002", EmptyList},
		{"note-robby-001", NonStandardType},
	}
	for _, c := range cases {
		if got := issueTypes(t, db, c.record); !slices.Contains(got, c.want) {
			t.Errorf("%s: issues %v, want %s", c.record, got, c.want)
		}
	}
	if v := scalar(t, db, `SELECT text FROM binaries WHERE id = 'binary-001'`); v.String != "Besides, if you jump on my shift, that's just rude man. - Dr. Robinavitch" {
		t.Errorf("binary-001 decoded to %q", v.String)
	}
	if v := scalar(t, db, `SELECT patient_id FROM medications WHERE id = 'med-nw-003'`); v.String != "noah-wyle" {
		t.Errorf("med-nw-003 linked to %q", v.String)
	}
}
