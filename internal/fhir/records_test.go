package fhir

import (
	"encoding/json"
	"testing"
)

func TestFirstSkipsEntriesWithoutCode(t *testing.T) {
	var c CodeableConcept
	json.Unmarshal([]byte(`{"text": "A1c",
		"coding": [{"display": "Hemoglobin A1c"}, {"system": "http://loinc.org", "code": "4548-4"}]}`), &c)
	got := c.First()
	if got.Code != "4548-4" || got.System != "http://loinc.org" || got.Text != "A1c" {
		t.Errorf("want the second entry's code with the label, got %+v", got)
	}

	var empty CodeableConcept
	json.Unmarshal([]byte(`{"text": "Sugar test", "coding": [{"display": "no code"}]}`), &empty)
	if got := empty.First(); got.Code != "" || got.Text != "Sugar test" {
		t.Errorf("want only the label, got %+v", got)
	}
}

func TestPreferPicksTheStandardCode(t *testing.T) {
	var c CodeableConcept
	json.Unmarshal([]byte(`{"text": "A1c", "coding": [
		{"system": "http://clinic.local/labs", "code": "HBA1C"},
		{"system": "http://loinc.org", "code": "4548-4", "display": "Hemoglobin A1c"}]}`), &c)
	if got := c.Prefer(LOINC); got.Code != "4548-4" || got.System != LOINC || got.Text != "A1c" {
		t.Errorf("want the LOINC code even though it is second, got %+v", got)
	}
	if got := c.Prefer(SNOMED); got.Code != "HBA1C" {
		t.Errorf("no SNOMED code: want the first code, got %+v", got)
	}

	var dx CodeableConcept
	json.Unmarshal([]byte(`{"coding": [
		{"system": "http://hl7.org/fhir/sid/icd-10-cm", "code": "E11.9"},
		{"system": "http://snomed.info/sct", "code": "44054006"}]}`), &dx)
	if got := dx.Prefer(SNOMED, ICD10); got.Code != "44054006" {
		t.Errorf("want SNOMED before ICD-10, got %+v", got)
	}
}
