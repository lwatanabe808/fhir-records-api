package fhir

// The FHIR fields the loader reads, one struct per record type. A record is
// decoded into its struct in one step (recordCtx.decode in the loader), so each handler can use
// p.BirthDate instead of looking fields up one at a time.
// Only the fields we use are listed. The rest stay in raw_resources.

// CodeableConcept is a coded value: a list of codes plus a free-text label.
type CodeableConcept struct {
	Coding []struct {
		System  string `json:"system"`
		Code    string `json:"code"`
		Display string `json:"display"`
	} `json:"coding"`
	Text string `json:"text"`
}

// First returns the first code in the list that has a code, plus the free-text label.
func (c CodeableConcept) First() Coding {
	result := Coding{Text: c.Text} // start with just the label

	for _, coding := range c.Coding { // go through each entry in the list
		if coding.Code == "" {
			continue // this entry has no code, try the next one
		}
		result.System = coding.System
		result.Code = coding.Code
		result.Display = coding.Display
		break // found one, stop looking
	}

	return result
}

// Standard code systems. US Core (the US rules for FHIR)
const (
	LOINC  = "http://loinc.org"                            // lab tests
	SNOMED = "http://snomed.info/sct"                      // diseases and procedures
	RxNorm = "http://www.nlm.nih.gov/research/umls/rxnorm" // drugs
	ICD10  = "http://hl7.org/fhir/sid/icd-10-cm"           // diseases, also allowed
	CPT    = "http://www.ama-assn.org/go/cpt"              // procedures, also allowed
)

// finds the first record that uses the standard code system
func (c CodeableConcept) Prefer(systems ...string) Coding {
	for _, system := range systems {
		for _, coding := range c.Coding {
			if coding.System == system && coding.Code != "" {
				return Coding{System: coding.System, Code: coding.Code, Display: coding.Display, Text: c.Text}
			}
		}
	}
	return c.First()
}

// Reference is a link to another record, like {"reference": "Patient/p1"}.
type Reference struct {
	Reference string `json:"reference"`
	Display   string `json:"display"` // a readable name, never used for linking
}

type Period struct {
	Start string `json:"start"`
}

type Meta struct {
	LastUpdated string `json:"lastUpdated"`
}

type HumanName struct {
	Family string   `json:"family"`
	Given  []string `json:"given"` // first and middle names
	Text   string   `json:"text"`  // the full name as it should be displayed
}

type Patient struct {
	Name             []HumanName `json:"name"`
	Gender           string      `json:"gender"`
	BirthDate        string      `json:"birthDate"`
	DeceasedDateTime string      `json:"deceasedDateTime"`
	Active           *bool       `json:"active"` // nil when missing
}

// Value holds the value fields we read from an observation or a component.
// FHIR allows 11 kinds of value (value[x]); these are the ones we store.
// Only one should be set; a pointer is nil when its field is missing.
type Value struct {
	ValueQuantity        *Quantity        `json:"valueQuantity"`
	ValueString          *string          `json:"valueString"`
	ValueBoolean         *bool            `json:"valueBoolean"`
	ValueInteger         *float64         `json:"valueInteger"`
	ValueCodeableConcept *CodeableConcept `json:"valueCodeableConcept"`
}

// Quantity is a number with its unit, like 7.8 %.
type Quantity struct {
	Value *float64 `json:"value"`
	Unit  string   `json:"unit"`
	Code  string   `json:"code"` // the unit as a code, e.g. "mm[Hg]"
}

// UnitName is the readable unit, or the unit code when that's all there is.
func (q Quantity) UnitName() string {
	if q.Unit != "" {
		return q.Unit
	}
	return q.Code
}

// Component is one part of a multi-part result, like systolic blood pressure.
type Component struct {
	Code CodeableConcept `json:"code"`
	Value
}

type Observation struct {
	Subject           *Reference      `json:"subject"` // the patient link; nil when missing
	Status            string          `json:"status"`
	Code              CodeableConcept `json:"code"`
	EffectiveDateTime string          `json:"effectiveDateTime"`
	EffectivePeriod   Period          `json:"effectivePeriod"`
	Issued            string          `json:"issued"`
	Meta              Meta            `json:"meta"`
	Value                             // the single result, if there is one
	Component         []Component     `json:"component"`
}

type Condition struct {
	Subject            *Reference      `json:"subject"` // the patient link; nil when missing
	Code               CodeableConcept `json:"code"`
	ClinicalStatus     CodeableConcept `json:"clinicalStatus"`
	VerificationStatus CodeableConcept `json:"verificationStatus"`
	OnsetDateTime      string          `json:"onsetDateTime"`
}

type MedicationRequest struct {
	Subject                   *Reference      `json:"subject"` // the patient link; nil when missing
	Status                    string          `json:"status"`
	Intent                    string          `json:"intent"`
	MedicationCodeableConcept CodeableConcept `json:"medicationCodeableConcept"`
	MedicationReference       any             `json:"medicationReference"` // a link to a Medication record, not supported yet
	AuthoredOn                string          `json:"authoredOn"`
	DosageInstruction         []struct {
		Text string `json:"text"`
	} `json:"dosageInstruction"`
}

type Procedure struct {
	Subject           *Reference      `json:"subject"` // the patient link; nil when missing
	Status            string          `json:"status"`
	Code              CodeableConcept `json:"code"`
	PerformedDateTime string          `json:"performedDateTime"`
	PerformedPeriod   Period          `json:"performedPeriod"`
	Performer         []struct {
		Actor Reference `json:"actor"`
	} `json:"performer"`
}

type Attachment struct {
	ContentType string  `json:"contentType"`
	URL         string  `json:"url"`
	Data        *string `json:"data"` // nil when missing
}

type DocumentReference struct {
	Subject *Reference      `json:"subject"` // the patient link; nil when missing
	Status  string          `json:"status"`
	Type    CodeableConcept `json:"type"`
	Date    string          `json:"date"`
	Author  []Reference     `json:"author"`
	Content []struct {
		Attachment Attachment `json:"attachment"`
	} `json:"content"`
}

type Binary struct {
	ContentType string `json:"contentType"`
	Data        string `json:"data"`
}
