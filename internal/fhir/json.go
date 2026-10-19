// Package fhir has small helpers for reading FHIR records.
//
// Records are decoded into map[string]any instead of typed structs on purpose.
// Clinic data is messy: a typed struct fails the whole record when one field has
// the wrong type (for example a value sent as "7.8" instead of 7.8). With a map we
// can read each field on its own, keep what is valid and flag what is not.
package fhir

import (
	"regexp"
	"time"
)

// Obj is one decoded JSON object.
type Obj = map[string]any

// Get walks a path of keys and list indexes, e.g. Get(r, "code", "coding", 0, "code").
// It returns nil if any step is missing or has the wrong shape.
func Get(v any, path ...any) any {
	cur := v
	for _, p := range path {
		switch key := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[key]
		case int:
			list, ok := cur.([]any)
			if !ok || key < 0 || key >= len(list) {
				return nil
			}
			cur = list[key]
		}
	}
	return cur
}

// Str returns the string at path, or "" if it is missing or not a string.
func Str(v any, path ...any) string {
	s, _ := Get(v, path...).(string)
	return s
}

// List returns the list at path, or nil.
func List(v any, path ...any) []any {
	l, _ := Get(v, path...).([]any)
	return l
}

// Num returns the number at path and whether it really was a number.
func Num(v any, path ...any) (float64, bool) {
	n, ok := Get(v, path...).(float64)
	return n, ok
}

// Coding is the first system/code/display found in a CodeableConcept,
// plus the concept's free text label.
type Coding struct {
	System  string
	Code    string
	Display string
	Text    string
}

// FirstCoding reads a CodeableConcept such as a lab test code, disease code or drug code.
func FirstCoding(concept any) Coding {
	return Coding{
		System:  Str(concept, "coding", 0, "system"),
		Code:    Str(concept, "coding", 0, "code"),
		Display: Str(concept, "coding", 0, "display"),
		Text:    Str(concept, "text"),
	}
}

// Name is the best readable name: the official display, then the clinic's label.
func (c Coding) Name() string {
	if c.Display != "" {
		return c.Display
	}
	return c.Text
}

// CodeOnly reads just the code of a CodeableConcept, e.g. clinicalStatus.
func CodeOnly(concept any) string {
	return Str(concept, "coding", 0, "code")
}

var refPattern = regexp.MustCompile(`^([A-Za-z]+)/([A-Za-z0-9\-.]{1,64})$`)

// ParseRef splits a relative reference like "Patient/noah-wyle" into type and id.
func ParseRef(ref string) (resourceType, id string, ok bool) {
	m := refPattern.FindStringSubmatch(ref)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// SubjectField is the field that holds the patient link. Most types use
// "subject", but some (Immunization, AllergyIntolerance...) use "patient".
func SubjectField(r Obj) string {
	if _, ok := r["subject"]; ok {
		return "subject"
	}
	if _, ok := r["patient"]; ok {
		return "patient"
	}
	return ""
}

var datePattern = regexp.MustCompile(`^\d{4}(-(0[1-9]|1[0-2])(-(0[1-9]|[12]\d|3[01]))?)?$`)

// ParseDate accepts the FHIR date and dateTime formats (year, year-month,
// date, or full timestamp with time zone). It returns the value normalized to
// UTC for timestamps, or as given for partial dates, and "" if invalid.
func ParseDate(s string) (normalized string, t time.Time, ok bool) {
	if s == "" {
		return "", time.Time{}, false
	}
	if datePattern.MatchString(s) {
		layout := map[int]string{4: "2006", 7: "2006-01", 10: "2006-01-02"}[len(s)]
		parsed, err := time.Parse(layout, s)
		if err != nil {
			return "", time.Time{}, false
		}
		return s, parsed, true
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", time.Time{}, false
	}
	return parsed.UTC().Format(time.RFC3339), parsed.UTC(), true
}
