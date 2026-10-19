package loader

import (
	"fmt"
	"sort"

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

func (rec *recordCtx) issue(issueType, severity, field, detail, action string) error {
	return rec.l.addIssue(Issue{
		RawResourceID: rec.line, RecordType: rec.resourceType, RecordID: rec.id,
		Type: issueType, Severity: severity, Field: field, Detail: detail, ActionTaken: action,
	})
}

// patientRef reads the patient link and checks its format.
// It returns "Patient/<id>" or "". The link itself is checked in pass 2,
// because the patient may appear later in the file.
func (rec *recordCtx) patientRef() (string, error) {
	field := fhir.SubjectField(rec.r)
	if field == "" {
		return "", rec.issue(NoPatientLink, Warning, "subject", "record has no patient link", "left unlinked")
	}
	if rec.flagged[field] || rec.flagged[field+"/reference"] {
		return "", nil // the schema check already reported a badly shaped link
	}
	subject, isObject := rec.r[field].(map[string]any)
	if !isObject {
		return "", rec.issue(InvalidReference, Warning, field, "patient link must be an object", "left unlinked")
	}
	ref, _ := subject["reference"].(string)
	if ref == "" {
		detail := "patient link has no reference"
		if name, _ := subject["display"].(string); name != "" {
			detail = fmt.Sprintf("patient link has only a name (%q), which is not unique", name)
		}
		return "", rec.issue(NoPatientLink, Warning, field, detail, "left unlinked, never matched by name")
	}
	refType, _, ok := fhir.ParseRef(ref)
	if !ok {
		return "", rec.issue(InvalidReference, Warning, field+".reference", fmt.Sprintf("%q is not in the form Patient/<id>", ref), "left unlinked")
	}
	if refType != "Patient" {
		return "", rec.issue(InvalidReference, Warning, field+".reference", fmt.Sprintf("%q does not point to a patient", ref), "left unlinked")
	}
	return ref, nil
}

// code reads a CodeableConcept (a lab test, disease, procedure or drug code)
// and reports a missing code or a missing code system.
func (rec *recordCtx) code(field, what string) (fhir.Coding, error) {
	concept, present := rec.r[field]
	c := fhir.FirstCoding(concept)
	if !present || c.Code == "" {
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

// date reads a date or dateTime field. Invalid dates become "".
// The schema check usually reports them; this catches the rest (like Feb 30).
func (rec *recordCtx) date(path ...any) (string, error) {
	raw := fhir.Str(rec.r, path...)
	if raw == "" {
		return "", nil
	}
	normalized, _, ok := fhir.ParseDate(raw)
	if ok {
		return normalized, nil
	}
	field := pathString(path)
	if rec.flagged[field] {
		return "", nil
	}
	return "", rec.issue(InvalidDate, Warning, field, fmt.Sprintf("%q is not a valid date", raw), "date stored as null")
}

// allowed returns the value if it is in the allowed set, otherwise "".
// dateOrStart reads a dateTime field, or the start of a period field when the
// dateTime is missing (FHIR allows either, e.g. effectiveDateTime or effectivePeriod).
func (rec *recordCtx) dateOrStart(dateField, periodField string) (string, error) {
	d, err := rec.date(dateField)
	if err != nil || d != "" {
		return d, err
	}
	return rec.date(periodField, "start")
}

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
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // stable issue order
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if err := rec.checkEmptyLists(t[k], p); err != nil {
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

func pathString(path []any) string {
	s := ""
	for _, p := range path {
		switch v := p.(type) {
		case string:
			if s != "" {
				s += "/"
			}
			s += v
		case int:
			s += fmt.Sprintf("/%d", v)
		}
	}
	return s
}
