package loader

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/lwatanabe808/novellia-fhir-api/internal/fhir"
	"github.com/lwatanabe808/novellia-fhir-api/internal/validate"
)

// handler holds the rules for one supported resource type.
// A type is supported only if it has a handler here. Every other FHIR type
// is kept in raw_resources and reported as UNSUPPORTED_TYPE.
type handler struct {
	insert func(rec *recordCtx) error
	remove func(tx *sql.Tx, id string) error // used when a newer copy of the same id arrives
}

var handlers = map[string]handler{
	"Patient":           {insertPatient, removeFrom("patients")},
	"Observation":       {insertObservation, removeObservation},
	"Condition":         {insertCondition, removeFrom("conditions")},
	"MedicationRequest": {insertMedication, removeFrom("medications")},
	"Procedure":         {insertProcedure, removeFrom("procedures")},
	"DocumentReference": {insertDocumentReference, removeDocument},
	"Binary":            {insertBinaryResource, removeFrom("binaries")},
}

// SupportedTypes lists the record types that have a handler, for building schema checkers.
func SupportedTypes() []string {
	var types []string
	for t := range handlers {
		types = append(types, t)
	}
	return types
}

func removeFrom(table string) func(*sql.Tx, string) error {
	return func(tx *sql.Tx, id string) error {
		_, err := tx.Exec(`
DELETE FROM `+table+`
WHERE id = ?`, id)
		return err
	}
}

func removeObservation(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`
DELETE FROM observation_values
WHERE observation_id = ?`, id); err != nil {
		return err
	}
	return removeFrom("observations")(tx, id)
}

func removeDocument(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`
DELETE FROM document_attachments
WHERE document_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`
DELETE FROM binaries
WHERE is_inline = 1
AND id LIKE ?`, id+"-inline-%"); err != nil {
		return err
	}
	return removeFrom("document_references")(tx, id)
}

// ---------- Patient ----------

func insertPatient(rec *recordCtx) error {
	var p fhir.Patient
	rec.decode(&p)

	// FHIR permits name parts, plain text, or both:
	// {given: [string, string], family: string}
	// {text: string}
	var name fhir.HumanName
	if len(p.Name) > 0 {
		name = p.Name[0]
	}
	var given []string // first and middle names
	for _, g := range name.Given {
		if g != "" {
			given = append(given, g)
		}
	}
	givenNames := strings.Join(given, " ")
	// text is the full name as it should be displayed (FHIR's definition), so use it
	// first, and build the name from the parts only when it's missing.
	fullName := name.Text
	if fullName == "" {
		fullName = strings.TrimSpace(givenNames + " " + name.Family)
	}

	birthDate, err := rec.date("birthDate", p.BirthDate)
	if err != nil {
		return err
	}
	deceased, err := rec.date("deceasedDateTime", p.DeceasedDateTime)
	if err != nil {
		return err
	}
	gender := allowed(p.Gender, "male", "female", "other", "unknown")
	if rec.flagged["active"] {
		p.Active = nil // wrong type: Go's decoder may have left false here
	}

	return rec.l.exec(`
INSERT INTO patients (id, raw_resource_id, full_name, family_name, given_names, gender, birth_date, deceased_date, active, name_key)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, fullName, name.Family, givenNames, gender, birthDate, deceased, p.Active,
		nameKey(fullName))
}

// ---------- Observation ----------
// Allowed values from FHIR R4's official value sets. The JSON schema only
// checks some of these, so all of them are listed here. They were copied
// from the pages below and kept in code, since HL7's full definitions file
// is too large to embed for a few short lists that don't change in R4.
// https://hl7.org/fhir/R4/valueset-observation-status.html
// https://hl7.org/fhir/R4/valueset-administrative-gender.html
// https://hl7.org/fhir/R4/valueset-document-reference-status.html
// https://hl7.org/fhir/R4/valueset-medicationrequest-status.html
// https://hl7.org/fhir/R4/valueset-medicationrequest-intent.html
// https://hl7.org/fhir/R4/valueset-event-status.html
// https://hl7.org/fhir/R4/valueset-condition-clinical.html
// https://hl7.org/fhir/R4/valueset-condition-ver-status.html
var observationStatuses = []string{"registered", "preliminary", "final", "amended", "corrected", "cancelled", "entered-in-error", "unknown"}

// These are the supported "value" fields per FHIR.
// FHIR allows only one value[x] per Observation. If a record has several,
// we keep the first one in this fixed order (Go maps don't keep JSON key order).
var allowedValueFields = []string{"valueQuantity", "valueString", "valueCodeableConcept", "valueBoolean", "valueInteger",
	"valueRange", "valueRatio", "valueSampledData", "valueTime", "valueDateTime", "valuePeriod"}

type obsValue struct {
	value     *float64
	valueText string
	unit      string
}

func insertObservation(rec *recordCtx) error {
	r := rec.r // the raw record, for the value fields
	var o fhir.Observation
	rec.decode(&o)

	patientRef, err := rec.patientRef(o.Subject)
	if err != nil {
		return err
	}
	code, err := rec.code("code", "lab test code", o.Code)
	if err != nil {
		return err
	}
	// FHIR allows a time (effectiveDateTime) or a period (effectivePeriod).
	measuredAt, err := rec.date("effectiveDateTime", o.EffectiveDateTime)
	if err != nil {
		return err
	}
	if measuredAt == "" {
		if measuredAt, err = rec.date("effectivePeriod/start", o.EffectivePeriod.Start); err != nil {
			return err
		}
	}
	lastUpdated, _ := rec.date("meta/lastUpdated", o.Meta.LastUpdated)
	issued, err := rec.date("issued", o.Issued)
	if err != nil {
		return err
	}
	status := allowed(o.Status, observationStatuses...)

	// Values can be in the parent (main) or nested (component)
	main, hasMainValue, err := rec.readFirstValue(r, o.Value, "")
	if err != nil {
		return err
	}

	if !hasMainValue && len(o.Component) == 0 {
		if err := rec.issue(MissingValue, Warning, "value", "observation has no value and no components", "value stored as null"); err != nil {
			return err
		}
	}

	// Can the value be real? Is the date in the future?
	impossible, err := rec.checkValue("value", main)
	if err != nil {
		return err
	}
	future, err := rec.checkFutureDate(measuredAt)
	if err != nil {
		return err
	}
	impossible = impossible || future

	// Components, e.g. the systolic and diastolic parts of a blood pressure.
	// They are checked before anything is saved, so the flags are final.
	type component struct {
		coding fhir.Coding
		value  obsValue
	}
	var comps []component
	for i, comp := range o.Component {
		label := fmt.Sprintf("component/%d", i) // same path format as the schema check
		v, _, err := rec.readFirstValue(fhir.Get(r, "component", i), comp.Value, label)
		if err != nil {
			return err
		}
		partImpossible, err := rec.checkValue(label, v)
		if err != nil {
			return err
		}
		impossible = impossible || partImpossible
		comps = append(comps, component{comp.Code.First(), v})
	}

	if err := rec.l.exec(`
INSERT INTO observations (id, raw_resource_id, patient_ref, code_system, lab_test_code, name, label, status, measured_at, last_updated, issued, is_impossible)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, patientRef, code.System, code.Code, code.Display, code.Text, status, measuredAt,
		lastUpdated, issued, impossible); err != nil {
		return err
	}

	// Save main and component values to values table
	if hasMainValue {
		if err := saveValue(rec, false, code, main); err != nil {
			return err
		}
	}
	for _, c := range comps {
		if err := saveValue(rec, true, c.coding, c.value); err != nil {
			return err
		}
	}
	return nil
}

// saveValue saves one result value of the observation into observation_values.
func saveValue(rec *recordCtx, isComponent bool, coding fhir.Coding, v obsValue) error {
	return rec.l.exec(`
INSERT INTO observation_values (observation_id, is_component, code_system, lab_test_code, name, value, value_text, unit)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, isComponent, coding.System, coding.Code, coding.Name(), v.value, v.valueText, v.unit)
}

// checkValue flags a number that can't be real, like a negative weight.
func (rec *recordCtx) checkValue(field string, v obsValue) (impossible bool, err error) {
	if v.value == nil {
		return false, nil
	}
	if reason := validate.CheckImpossible(*v.value, v.unit); reason != "" {
		return true, rec.issue(ImpossibleValue, Error, field, reason, "hidden from default views and research")
	}
	return false, nil
}

// checkFutureDate flags a measurement dated after today, which can't be real.
func (rec *recordCtx) checkFutureDate(measuredAt string) (future bool, err error) {
	_, t, ok := fhir.ParseDate(measuredAt)
	if !ok || !t.After(rec.l.cfg.Now()) {
		return false, nil
	}
	return true, rec.issue(ImpossibleValue, Error, "effectiveDateTime", "measurement is dated in the future", "hidden from default views and research")
}

// readFirstValue reads the value of an observation (label "") or of one of its
// components (label "component/0"). found is false when there is no value.
// FHIR allows one value; if there are several, it reports that and keeps the first.
// raw is the same observation or component as a loose map, used to see which
// value fields it has (a struct can't tell a missing field from an empty one).
func (rec *recordCtx) readFirstValue(raw any, val fhir.Value, label string) (v obsValue, found bool, err error) {
	fields := valueFields(raw)
	if len(fields) == 0 {
		return v, false, nil
	}
	if len(fields) > 1 {
		if err := rec.issue(MultipleValues, Warning, joinSlash(label, strings.Join(fields, ", ")),
			"FHIR allows one value", "kept "+fields[0]); err != nil {
			return v, true, err
		}
	}
	v, err = rec.readValue(val, fields[0], joinSlash(label, fields[0]))
	return v, true, err
}

// valueFields lists the value fields an observation or component has,
// in allowedValueFields order.
func valueFields(obj any) []string {
	m, _ := obj.(map[string]any)
	var fields []string
	for _, f := range allowedValueFields {
		if _, ok := m[f]; ok {
			fields = append(fields, f)
		}
	}
	return fields
}

// joinSlash builds a field path like the schema check does: "component/0" + "valueQuantity".
func joinSlash(path, field string) string {
	if path == "" {
		return field
	}
	return path + "/" + field
}

// for each observation, when we read the observation, we only take the
// first value
func (rec *recordCtx) readValue(val fhir.Value, field, path string) (obsValue, error) {
	var v obsValue
	// values flagged by schema check
	if rec.flagged[path] {
		return v, nil
	}
	switch field {
	// valueQuantity typically has value and unit
	case "valueQuantity":
		q := val.ValueQuantity
		if q == nil || rec.flagged[path+"/value"] {
			break // no number, or not a number (already reported by the schema check)
		}
		v.value, v.unit = q.Value, q.UnitName()
		if v.value != nil && v.unit == "" {
			return v, rec.issue(MissingUnit, Warning, path+".unit", "number has no unit", "unit stored as null")
		}
	case "valueInteger":
		v.value = val.ValueInteger
	case "valueString":
		if val.ValueString != nil {
			v.valueText = *val.ValueString
		}
	case "valueBoolean":
		if val.ValueBoolean != nil {
			v.valueText = strconv.FormatBool(*val.ValueBoolean)
		}
	case "valueCodeableConcept":
		if val.ValueCodeableConcept != nil {
			v.valueText = val.ValueCodeableConcept.First().Name()
		}
	default:
		// Valid FHIR we don't store yet, like valueRange. Log it so the value
		// isn't lost silently; the original stays in raw_resources.
		return v, rec.issue(UnsupportedValueType, Info, path, field+" is valid FHIR but not stored yet", "value stored as null, kept in raw_resources")
	}
	return v, nil
}

// ---------- Condition ----------

func insertCondition(rec *recordCtx) error {
	var c fhir.Condition
	rec.decode(&c)

	patientRef, err := rec.patientRef(c.Subject)
	if err != nil {
		return err
	}
	code, err := rec.code("code", "disease code", c.Code)
	if err != nil {
		return err
	}
	onset, err := rec.date("onsetDateTime", c.OnsetDateTime)
	if err != nil {
		return err
	}
	// take the first status code and check if it's one of the valid statuses.
	clinical := allowed(c.ClinicalStatus.First().Code, "active", "recurrence", "relapse", "inactive", "remission", "resolved")
	verification := allowed(c.VerificationStatus.First().Code, "unconfirmed", "provisional", "differential", "confirmed", "refuted", "entered-in-error")
	return rec.l.exec(`
INSERT INTO conditions (id, raw_resource_id, patient_ref, code_system, disease_code, name, label, clinical_status, verification_status, onset_date)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, patientRef, code.System, code.Code,
		code.Display, code.Text, clinical, verification, onset)
}

// ---------- MedicationRequest ----------

func insertMedication(rec *recordCtx) error {
	var m fhir.MedicationRequest
	rec.decode(&m)

	patientRef, err := rec.patientRef(m.Subject)
	if err != nil {
		return err
	}
	var code fhir.Coding
	if m.MedicationReference != nil {
		if err := rec.issue(MissingCode, Warning, "medicationReference",
			"drug is a link to a Medication record, which is not supported yet", "drug code stored as null"); err != nil {
			return err
		}
	} else if code, err = rec.code("medicationCodeableConcept", "drug code", m.MedicationCodeableConcept); err != nil {
		return err
	}
	prescribedAt, err := rec.date("authoredOn", m.AuthoredOn)
	if err != nil {
		return err
	}
	var dosage []string
	for _, d := range m.DosageInstruction {
		if d.Text != "" {
			dosage = append(dosage, d.Text)
		}
	}
	status := allowed(m.Status, "active", "on-hold", "cancelled", "completed", "entered-in-error", "stopped", "draft", "unknown")
	intent := allowed(m.Intent, "proposal", "plan", "order", "original-order", "reflex-order", "filler-order", "instance-order", "option")
	return rec.l.exec(`
INSERT INTO medications (id, raw_resource_id, patient_ref, code_system, drug_code, name, label, status, intent, prescribed_at, dosage)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, patientRef, code.System, code.Code,
		code.Display, code.Text, status, intent,
		prescribedAt, strings.Join(dosage, "; "))
}

// ---------- Procedure ----------

func insertProcedure(rec *recordCtx) error {
	var p fhir.Procedure
	rec.decode(&p)

	patientRef, err := rec.patientRef(p.Subject)
	if err != nil {
		return err
	}
	code, err := rec.code("code", "procedure code", p.Code)
	if err != nil {
		return err
	}
	// FHIR allows a time (performedDateTime) or a period (performedPeriod).
	performedAt, err := rec.date("performedDateTime", p.PerformedDateTime)
	if err != nil {
		return err
	}
	if performedAt == "" {
		if performedAt, err = rec.date("performedPeriod/start", p.PerformedPeriod.Start); err != nil {
			return err
		}
	}
	var performer string
	if len(p.Performer) > 0 {
		performer = p.Performer[0].Actor.Reference
	}
	status := allowed(p.Status, "preparation", "in-progress", "not-done", "on-hold", "stopped", "completed", "entered-in-error", "unknown")
	return rec.l.exec(`
INSERT INTO procedures (id, raw_resource_id, patient_ref, code_system, procedure_code, name, label, status, performed_at, performer)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, patientRef, code.System, code.Code,
		code.Display, code.Text, status, performedAt, performer)
}
