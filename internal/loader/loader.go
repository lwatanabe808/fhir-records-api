// Package loader reads the JSONL file into SQLite.
//
// It runs in two passes:
//
//	Pass 1: read each line, check it, clean it and insert it. Anything that can
//	        be decided from one line alone is decided here.
//	Pass 2: once every line is in the database, connect links (patients,
//	        binaries) and compare records (duplicates, corrections, orphans).
//	        This has to wait, because a record can link to one that appears
//	        later in the file.
//
// The loader never stops on bad data. Every problem becomes a row in the
// issues table, and the original line is always kept in raw_resources.
package loader

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lwatanabe808/novellia-fhir-api/internal/fhir"
	"github.com/lwatanabe808/novellia-fhir-api/internal/store"
	"github.com/lwatanabe808/novellia-fhir-api/internal/validate"
)

// Config holds the limits and rules the loader uses.
type Config struct {
	MaxBinaryBytes int             // decoded Binary data over this size is not stored
	MaxRecordBytes int             // non-Binary lines over this size are not processed
	Ranges         validate.Ranges // plausible ranges per lab test code
	Now            func() time.Time
}

// Summary is what the loader reports when it finishes.
type Summary struct {
	Lines      int            `json:"lines"`
	BlankLines int            `json:"blankLines"`
	Loaded     int            `json:"loaded"`
	RawOnly    int            `json:"rawOnly"`
	Failed     int            `json:"failed"`
	ByType     map[string]int `json:"byType"`
	IssueCount int            `json:"issues"`
	DurationMS int64          `json:"durationMs"`
	SourceFile string         `json:"sourceFile"`
}

// Loader holds the state of one load.
type Loader struct {
	tx      *sql.Tx
	schema  *validate.Schema
	cfg     Config
	summary Summary

	// seen remembers the first copy of each type + id, to detect duplicates.
	seen map[string]seenRecord
}

type seenRecord struct {
	line        int64
	canonical   []byte // the record re-encoded with sorted keys, for comparing content
	lastUpdated string
}

// LoadFile loads one JSONL file into the database.
func LoadFile(ctx context.Context, db *sql.DB, path string, schema *validate.Schema, cfg Config) (Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return Summary{}, fmt.Errorf("open data file: %w", err)
	}
	defer f.Close()
	s, err := Load(ctx, db, f, schema, cfg)
	s.SourceFile = path
	return s, err
}

// Load reads JSONL from r. Everything runs in one transaction, which keeps
// the load fast and means the API never sees a half-loaded database.
func Load(ctx context.Context, db *sql.DB, r io.Reader, schema *validate.Schema, cfg Config) (Summary, error) {
	start := time.Now()
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Summary{}, err
	}
	defer tx.Rollback()

	l := &Loader{
		tx:      tx,
		schema:  schema,
		cfg:     cfg,
		summary: Summary{ByType: map[string]int{}},
		seen:    map[string]seenRecord{},
	}

	// Pass 1. ReadBytes has no line length limit, unlike bufio.Scanner,
	// so one huge line can't stop the load.
	br := bufio.NewReader(r)
	var lineNo int64
	for {
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			if err := l.processLine(lineNo, line); err != nil {
				return l.summary, fmt.Errorf("line %d: %w", lineNo, err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return l.summary, fmt.Errorf("read data file: %w", readErr)
		}
	}

	// Pass 2.
	if err := l.secondPass(); err != nil {
		return l.summary, fmt.Errorf("second pass: %w", err)
	}

	if err := tx.QueryRow(`SELECT COUNT(*) FROM issues`).Scan(&l.summary.IssueCount); err != nil {
		return l.summary, err
	}
	if err := tx.Commit(); err != nil {
		return l.summary, err
	}
	l.summary.DurationMS = time.Since(start).Milliseconds()
	return l.summary, nil
}

// processLine handles one line of the file. It only returns an error for
// database failures. Bad data is recorded as issues, never returned as errors.
func (l *Loader) processLine(lineNo int64, line []byte) error {
	l.summary.Lines++
	line = bytes.TrimSpace(line) // also removes Windows "\r" line endings
	if len(line) == 0 {
		l.summary.BlankLines++
		return nil
	}

	// Step 1: parse. One bad line never affects the others.
	var parsed any
	if err := json.Unmarshal(line, &parsed); err != nil {
		l.summary.Failed++
		if _, err := l.tx.Exec(`INSERT INTO raw_resources (id, parsed_ok, raw_text) VALUES (?, 0, ?)`, lineNo, string(line)); err != nil {
			return err
		}
		return l.addIssue(Issue{RawResourceID: lineNo, Type: InvalidJSON, Severity: Error,
			Detail: err.Error(), ActionTaken: "skipped line"})
	}

	// Valid JSON, but is it a record at all?
	record, isObject := parsed.(map[string]any)
	resourceType, _ := record["resourceType"].(string)
	if !isObject || resourceType == "" {
		l.summary.Failed++
		if _, err := l.tx.Exec(`INSERT INTO raw_resources (id, parsed_ok, raw_text) VALUES (?, 1, ?)`, lineNo, string(line)); err != nil {
			return err
		}
		return l.addIssue(Issue{RawResourceID: lineNo, Type: MalformedRecord, Severity: Error,
			Detail: "line is valid JSON but not an object with a resourceType", ActionTaken: "stored raw only"})
	}
	id, _ := record["id"].(string)
	l.summary.ByType[resourceType]++

	rec := &recordCtx{l: l, line: lineNo, resourceType: resourceType, id: id, r: record, raw: line, flagged: map[string]bool{}}

	// Very large non-Binary lines are kept as a stub and not processed.
	if len(line) > l.cfg.MaxRecordBytes && resourceType != "Binary" {
		l.summary.RawOnly++
		if err := l.insertRaw(rec, false); err != nil {
			return err
		}
		return rec.issue(RecordTooLarge, Warning, "", fmt.Sprintf("line is %d bytes", len(line)), "stored type and id only")
	}

	if err := l.insertRaw(rec, true); err != nil {
		return err
	}

	// Step 2: is this a real FHIR type?
	if !l.schema.IsFHIRType(resourceType) {
		l.summary.RawOnly++
		return rec.issue(NonStandardType, Warning, "resourceType", resourceType+" is not a FHIR R4 resource type", "stored raw only")
	}
	if id == "" {
		l.summary.RawOnly++
		return rec.issue(MissingID, Error, "id", "record has no id, so nothing can link to it", "stored raw only")
	}
	h, supported := handlers[resourceType]
	if !supported {
		l.summary.RawOnly++
		return rec.issue(UnsupportedType, Info, "resourceType", resourceType+" is valid FHIR but not supported yet", "stored raw only, available at /patients/{id}/records")
	}

	// Same type + id seen before?
	skip, err := l.checkDuplicateID(rec, h)
	if err != nil || skip {
		return err
	}

	// FHIR schema check.
	if err := l.schemaCheck(rec); err != nil {
		return err
	}
	if err := rec.checkEmptyLists(record, ""); err != nil {
		return err
	}

	// Step 3: the type's own rules. A panic in cleaning code becomes an issue,
	// so one unexpected record shape can't stop the load.
	if err := l.runHandler(rec, h); err != nil {
		return err
	}
	l.summary.Loaded++
	return nil
}

func (l *Loader) runHandler(rec *recordCtx, h handler) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = rec.issue(ProcessingError, Error, "", fmt.Sprint(p), "record skipped")
		}
	}()
	if herr := h.insert(rec); herr != nil {
		if isDBError(herr) {
			return herr
		}
		return rec.issue(ProcessingError, Error, "", herr.Error(), "record skipped")
	}
	return nil
}

// dbError marks errors that should stop the load (the database itself failed).
type dbError struct{ err error }

func (e dbError) Error() string { return e.err.Error() }
func (e dbError) Unwrap() error { return e.err }

func isDBError(err error) bool {
	var d dbError
	return errors.As(err, &d)
}

func (l *Loader) insertRaw(rec *recordCtx, keepJSON bool) error {
	patientRef := ""
	if field := fhir.SubjectField(rec.r); field != "" {
		patientRef = fhir.Str(rec.r, field, "reference")
	}
	var rawJSON any
	if keepJSON {
		rawJSON = string(rec.raw)
	}
	_, err := l.tx.Exec(`INSERT INTO raw_resources (id, resource_type, fhir_id, patient_ref, parsed_ok, raw_json)
		VALUES (?, ?, ?, ?, 1, ?)`, rec.line, rec.resourceType, store.Nullable(rec.id), store.Nullable(patientRef), rawJSON)
	return err
}

// checkDuplicateID handles a type + id that was already loaded.
// Identical copy: skip it. Changed copy: the newer version replaces the old one.
func (l *Loader) checkDuplicateID(rec *recordCtx, h handler) (skip bool, err error) {
	key := rec.resourceType + "/" + rec.id
	canonical, _ := json.Marshal(rec.r) // Go sorts map keys, so equal content gives equal bytes
	lastUpdated := fhir.Str(rec.r, "meta", "lastUpdated")
	prev, seen := l.seen[key]
	if !seen {
		l.seen[key] = seenRecord{line: rec.line, canonical: canonical, lastUpdated: lastUpdated}
		return false, nil
	}
	if bytes.Equal(prev.canonical, canonical) {
		return true, rec.issue(DuplicateID, Info, "id", fmt.Sprintf("identical copy of line %d", prev.line), "skipped copy")
	}

	// Different content. The later line wins, unless meta.lastUpdated says the earlier one is newer.
	if prev.lastUpdated != "" && lastUpdated != "" {
		_, prevT, ok1 := fhir.ParseDate(prev.lastUpdated)
		_, curT, ok2 := fhir.ParseDate(lastUpdated)
		if ok1 && ok2 && prevT.After(curT) {
			return true, rec.issue(DuplicateIDChanged, Warning, "id",
				fmt.Sprintf("changed copy of line %d, but line %d has a newer meta.lastUpdated", prev.line, prev.line), "kept the newer version")
		}
	}
	if err := h.remove(rec.l.tx, rec.id); err != nil {
		return false, err
	}
	l.seen[key] = seenRecord{line: rec.line, canonical: canonical, lastUpdated: lastUpdated}
	return false, rec.issue(DuplicateIDChanged, Warning, "id", fmt.Sprintf("replaces the version on line %d", prev.line), "kept the later version")
}

// schemaCheck validates the record against the official FHIR schema and turns
// each error into an issue. Common errors get their own issue type.
func (l *Loader) schemaCheck(rec *recordCtx) error {
	problems, err := l.schema.Validate(rec.resourceType, rec.raw)
	if err != nil {
		return rec.issue(ProcessingError, Error, "", "schema check failed: "+err.Error(), "continued without schema check")
	}
	for _, p := range problems {
		issueType := InvalidFHIR
		action := "kept the record"
		field := strings.TrimPrefix(p.Path, "/")
		switch {
		case p.Keyword == "required" && strings.Contains(p.Message, "code"):
			continue // a missing code is reported as MISSING_CODE by the type's own rules
		case p.Keyword == "enum" && strings.HasSuffix(strings.ToLower(field), "status"):
			issueType, action = InvalidStatus, "status stored as null"
		case p.Keyword == "pattern" && isDateField(field):
			issueType, action = InvalidDate, "date stored as null"
		case p.Keyword == "type" && strings.Contains(field, "value"):
			issueType, action = InvalidValueType, "value stored as null"
		case p.Keyword == "type" && (field == "subject" || field == "patient"):
			issueType, action = InvalidReference, "left unlinked"
		}
		rec.flagged[field] = true
		if err := rec.issue(issueType, Warning, field, p.Message, action); err != nil {
			return err
		}
	}
	return nil
}

func isDateField(field string) bool {
	last := field[strings.LastIndex(field, "/")+1:]
	switch last {
	case "birthDate", "date", "issued", "authoredOn", "recordedDate", "lastUpdated", "start", "end":
		return true
	}
	return strings.HasSuffix(last, "DateTime")
}

func (l *Loader) addIssue(i Issue) error {
	_, err := l.tx.Exec(`INSERT INTO issues (raw_resource_id, record_type, record_id, issue_type, severity, field, detail, action_taken, group_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullableInt(i.RawResourceID), store.Nullable(i.RecordType), store.Nullable(i.RecordID), i.Type, i.Severity,
		store.Nullable(i.Field), store.Nullable(i.Detail), store.Nullable(i.ActionTaken), store.Nullable(i.GroupKey))
	if err != nil {
		return dbError{err}
	}
	return nil
}

func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
