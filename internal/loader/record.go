package loader

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/lwatanabe808/novellia-fhir-api/internal/fhir"
)

// recordCtx is one record being processed, with helpers for logging issues.
type recordCtx struct {
	l            *Loader
	line         int64
	resourceType string
	id           string
	r            fhir.Obj
	raw          []byte

	// flagged holds fields the schema check already reported, so the type's
	// own rules don't report the same problem twice.
	flagged map[string]bool
}

// decode fills a record struct (fhir.Patient, fhir.Observation...) from the line
// in one step. If a field has the wrong type, Go leaves it empty and fills in
// the rest; the schema check has already reported that field, so the error is ignored.
func (rec *recordCtx) decode(v any) {
	_ = json.Unmarshal(rec.raw, v)
}

func (rec *recordCtx) issue(issueType, severity, field, detail, action string) error {
	return rec.l.addIssue(Issue{
		RawResourceID: rec.line, RecordType: rec.resourceType, RecordID: rec.id,
		Type: issueType, Severity: severity, Field: field, Detail: detail, ActionTaken: action,
	})
}

// patientRef checks the patient link and returns "Patient/<id>", or "".
// Whether that patient exists is checked in pass 2, because the patient may
// appear later in the file.
func (rec *recordCtx) patientRef(subject *fhir.Reference) (string, error) {
	if rec.flagged["subject"] || rec.flagged["subject/reference"] {
		return "", nil // the schema check already reported a badly shaped link
	}
	raw, present := rec.r["subject"]
	if !present {
		return "", rec.issue(NoPatientLink, Warning, "subject", "record has no patient link", "left unlinked")
	}
	// The schema check doesn't catch a link written as plain text ("subject": "Patient/p1")
	// or null, and the struct can't tell those apart from an empty link, so the shape
	// is checked on the raw record.
	if _, isObject := raw.(map[string]any); !isObject || subject == nil {
		return "", rec.issue(InvalidReference, Warning, "subject", "patient link must be an object", "left unlinked")
	}
	if subject.Reference == "" {
		detail := "patient link has no reference"
		if subject.Display != "" {
			detail = fmt.Sprintf("patient link has only a name (%q), which is not unique", subject.Display)
		}
		return "", rec.issue(NoPatientLink, Warning, "subject", detail, "left unlinked, never matched by name")
	}
	refType, _, ok := fhir.ParseRef(subject.Reference)
	if !ok {
		return "", rec.issue(InvalidReference, Warning, "subject.reference", fmt.Sprintf("%q is not in the form Patient/<id>", subject.Reference), "left unlinked")
	}
	if refType != "Patient" {
		return "", rec.issue(InvalidReference, Warning, "subject.reference", fmt.Sprintf("%q does not point to a patient", subject.Reference), "left unlinked")
	}
	return subject.Reference, nil
}

// code checks a CodeableConcept (a lab test, disease, procedure or drug code)
// and reports a missing code or a missing code system.
func (rec *recordCtx) code(field, what string, concept fhir.CodeableConcept) (fhir.Coding, error) {
	// finds the first code that has a code
	c := concept.First()
	if c.Code == "" {
		detail := fmt.Sprintf("no %s", what)
		if c.Text != "" {
			detail += fmt.Sprintf(" (only the label %q)", c.Text)
		}
		return c, rec.issue(MissingCode, Warning, field, detail, "code stored as null")
	}
	if c.System == "" {
		return c, rec.issue(MissingCodeSystem, Warning, field+".coding.system",
			fmt.Sprintf("%s %q has no code system, so its meaning is unclear", what, c.Code), "code system stored as null")
	}
	return c, nil
}

// date checks a date or dateTime value. Invalid dates become "".
// The schema check usually reports them; this catches the rest (like Feb 30).
// field is the value's path in the record, e.g. "birthDate" or "effectivePeriod/start".
func (rec *recordCtx) date(field, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	normalized, _, ok := fhir.ParseDate(raw)
	if ok {
		return normalized, nil
	}
	if rec.flagged[field] {
		return "", nil
	}
	return "", rec.issue(InvalidDate, Warning, field, fmt.Sprintf("%q is not a valid date", raw), "date stored as null")
}

// allowed returns the value if it is in the allowed set, otherwise "".
// Invalid statuses are reported by the schema check; here we only keep them out of the clean table.
func allowed(value string, set ...string) string {
	for _, s := range set {
		if value == s {
			return value
		}
	}
	return ""
}

// checkEmptyLists reports every empty list in the record. FHIR does not allow
// them, but the official schema doesn't check for them.
func (rec *recordCtx) checkEmptyLists(v any, path string) error {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) { // sorted, so issues come out in the same order every time
			if err := rec.checkEmptyLists(t[k], joinPath(path, k)); err != nil {
				return err
			}
		}
	case []any:
		if len(t) == 0 {
			return rec.issue(EmptyList, Warning, path, "empty list (FHIR lists may not be empty)", "stored as null")
		}
		for i, item := range t {
			if err := rec.checkEmptyLists(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// joinPath adds a field name to a path: "" + "name" = "name", "name" + "given" = "name.given".
func joinPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}
