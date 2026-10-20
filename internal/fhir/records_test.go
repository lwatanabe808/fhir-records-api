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
