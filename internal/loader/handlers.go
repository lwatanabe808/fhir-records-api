package loader

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/lwatanabe808/novellia-fhir-api/internal/fhir"
	"github.com/lwatanabe808/novellia-fhir-api/internal/store"
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

func exec(rec *recordCtx, query string, args ...any) error {
	if _, err := rec.l.tx.Exec(query, args...); err != nil {
		return dbError{err}
	}
	return nil
}

func removeFrom(table string) func(*sql.Tx, string) error {
	return func(tx *sql.Tx, id string) error {
		_, err := tx.Exec(`DELETE FROM `+table+` WHERE id = ?`, id)
		return err
	}
}

func removeObservation(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`DELETE FROM observation_components WHERE observation_id = ?`, id); err != nil {
		return err
	}
	return removeFrom("observations")(tx, id)
}

func removeDocument(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`DELETE FROM document_attachments WHERE document_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM binaries WHERE is_inline = 1 AND id LIKE ?`, id+"-inline-%"); err != nil {
		return err
	}
	return removeFrom("document_references")(tx, id)
}

// ---------- Patient ----------

func insertPatient(rec *recordCtx) error {
	r := rec.r
	family := fhir.Str(r, "name", 0, "family")
	var given []string
	for _, g := range fhir.List(r, "name", 0, "given") {
		if s, ok := g.(string); ok {
			given = append(given, s)
		}
	}
	givenNames := strings.Join(given, " ")
	fullName := strings.TrimSpace(givenNames + " " + family)
	if text := fhir.Str(r, "name", 0, "text"); fullName == "" && text != "" {
		fullName = text
	}
	birthDate, err := rec.date("birthDate")
	if err != nil {
		return err
	}
	deceased, err := rec.date("deceasedDateTime")
	if err != nil {
		return err
	}
	gender := allowed(fhir.Str(r, "gender"), "male", "female", "other", "unknown")
	var active any
	if b, ok := r["active"].(bool); ok {
		active = b
	}
	return exec(rec, `INSERT INTO patients (id, raw_resource_id, full_name, family_name, given_names, gender, birth_date, deceased_date, active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, store.Nullable(fullName), store.Nullable(family), store.Nullable(givenNames),
		store.Nullable(gender), store.Nullable(birthDate), store.Nullable(deceased), active)
}

// ---------- Observation ----------

var observationStatuses = []string{"registered", "preliminary", "final", "amended", "corrected", "cancelled", "entered-in-error", "unknown"}

// FHIR allows only one value[x] per Observation. If a record has several,
// we keep the first one in this fixed order (Go maps don't keep JSON key order).
var valueFields = []string{"valueQuantity", "valueString", "valueCodeableConcept", "valueBoolean", "valueInteger",
	"valueRange", "valueRatio", "valueSampledData", "valueTime", "valueDateTime", "valuePeriod"}

type obsValue struct {
	value     *float64
	valueText string
	unit      string
}

func insertObservation(rec *recordCtx) error {
	r := rec.r
	patientRef, err := rec.patientRef()
	if err != nil {
		return err
	}
	code, err := rec.code("code", "lab test code")
	if err != nil {
		return err
	}
	measuredAt, err := rec.date("effectiveDateTime")
	if err != nil {
		return err
	}
	if measuredAt == "" {
		if measuredAt, err = rec.date("effectivePeriod", "start"); err != nil {
			return err
		}
	}
	lastUpdated, _ := rec.date("meta", "lastUpdated")
	issued, err := rec.date("issued")
	if err != nil {
		return err
	}
	status := allowed(fhir.Str(r, "status"), observationStatuses...)

	// The main value.
	var present []string
	for _, f := range valueFields {
		if _, ok := r[f]; ok {
			present = append(present, f)
		}
	}
	var main obsValue
	if len(present) > 1 {
		if err := rec.issue(MultipleValues, Warning, strings.Join(present, ", "),
			"FHIR allows one value per observation", "kept "+present[0]); err != nil {
			return err
		}
	}
	if len(present) > 0 {
		if main, err = rec.readValue(r, present[0], present[0]); err != nil {
			return err
		}
	}

	components := fhir.List(r, "component")
	_, hasAbsentReason := r["dataAbsentReason"]
	if len(present) == 0 && len(components) == 0 && !hasAbsentReason {
		if err := rec.issue(MissingValue, Warning, "value", "observation has no value and no components", "value stored as null"); err != nil {
			return err
		}
	}

	// Impossible and implausible values.
	impossible, implausible := false, false
	check := func(c, label string, v obsValue) error {
		if v.value == nil {
			return nil
		}
		if reason := validate.CheckImpossible(*v.value, v.unit); reason != "" {
			impossible = true
			return rec.issue(ImpossibleValue, Error, label, reason, "hidden from default views and research")
		}
		if reason := rec.l.cfg.Ranges.CheckImplausible(c, *v.value, v.unit); reason != "" {
			implausible = true
			return rec.issue(ImplausibleValue, Warning, label, reason+" (placeholder range, needs clinical review)", "shown with a warning, excluded from research")
		}
		return nil
	}
	if err := check(code.Code, "value", main); err != nil {
		return err
	}
	if measuredAt != "" {
		if _, t, ok := fhir.ParseDate(measuredAt); ok && t.After(rec.l.cfg.Now()) {
			impossible = true
			if err := rec.issue(ImpossibleValue, Error, "effectiveDateTime", "measurement is dated in the future", "hidden from default views and research"); err != nil {
				return err
			}
		}
	}

	if err := exec(rec, `INSERT INTO observations (id, raw_resource_id, patient_ref, code_system, lab_test_code, name, label,
			status, measured_at, value, value_text, unit, last_updated, issued, is_impossible, is_implausible)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, store.Nullable(patientRef), store.Nullable(code.System), store.Nullable(code.Code),
		store.Nullable(code.Display), store.Nullable(code.Text), store.Nullable(status), store.Nullable(measuredAt),
		main.value, store.Nullable(main.valueText), store.Nullable(main.unit),
		store.Nullable(lastUpdated), store.Nullable(issued), impossible, implausible); err != nil {
		return err
	}

	// Components, e.g. the systolic and diastolic parts of a blood pressure.
	var systolic, diastolic *float64
	for i, comp := range components {
		c := fhir.FirstCoding(fhir.Get(comp, "code"))
		label := fmt.Sprintf("component/%d", i) // same path format as the schema check
		var v obsValue
		for _, f := range valueFields {
			if _, ok := comp.(map[string]any)[f]; ok {
				if v, err = rec.readValue(comp, f, label+"/"+f); err != nil {
					return err
				}
				break
			}
		}
		if err := check(c.Code, label, v); err != nil {
			return err
		}
		switch c.Code {
		case "8480-6":
			systolic = v.value
		case "8462-4":
			diastolic = v.value
		}
		if err := exec(rec, `INSERT INTO observation_components (observation_id, code_system, lab_test_code, name, value, value_text, unit)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			rec.id, store.Nullable(c.System), store.Nullable(c.Code), store.Nullable(c.Name()),
			v.value, store.Nullable(v.valueText), store.Nullable(v.unit)); err != nil {
			return err
		}
	}
	if systolic != nil && diastolic != nil && *systolic <= *diastolic {
		impossible = true
		if err := rec.issue(ImpossibleValue, Error, "component",
			fmt.Sprintf("systolic %v is not higher than diastolic %v", *systolic, *diastolic), "hidden from default views and research"); err != nil {
			return err
		}
	}
	// Component checks may have changed the flags after the row was inserted.
	return exec(rec, `UPDATE observations SET is_impossible = ?, is_implausible = ? WHERE id = ?`, impossible, implausible, rec.id)
}

// readValue reads one value[x] field from an observation or component.
func (rec *recordCtx) readValue(obj any, field, label string) (obsValue, error) {
	var v obsValue
	raw := fhir.Get(obj, field)
	switch field {
	case "valueQuantity":
		if n, ok := fhir.Num(raw, "value"); ok {
			v.value = &n
		} else if fhir.Get(raw, "value") != nil && !rec.flagged[label+"/value"] {
			if err := rec.issue(InvalidValueType, Warning, label+".value", "value is not a number", "value stored as null"); err != nil {
				return v, err
			}
		}
		v.unit = fhir.Str(raw, "unit")
		if v.unit == "" {
			v.unit = fhir.Str(raw, "code")
		}
		if v.unit == "" && v.value != nil {
			if err := rec.issue(MissingUnit, Warning, label+".unit", "number has no unit", "unit stored as null"); err != nil {
				return v, err
			}
		}
	case "valueInteger":
		if n, ok := raw.(float64); ok {
			v.value = &n
		}
	case "valueString":
		v.valueText, _ = raw.(string)
	case "valueBoolean":
		if b, ok := raw.(bool); ok {
			v.valueText = fmt.Sprint(b)
		}
	case "valueCodeableConcept":
		v.valueText = fhir.FirstCoding(raw).Name()
	}
	return v, nil
}

// ---------- Condition ----------

func insertCondition(rec *recordCtx) error {
	r := rec.r
	patientRef, err := rec.patientRef()
	if err != nil {
		return err
	}
	code, err := rec.code("code", "disease code")
	if err != nil {
		return err
	}
	onset, err := rec.date("onsetDateTime")
	if err != nil {
		return err
	}
	clinical := allowed(fhir.CodeOnly(r["clinicalStatus"]), "active", "recurrence", "relapse", "inactive", "remission", "resolved")
	verification := allowed(fhir.CodeOnly(r["verificationStatus"]), "unconfirmed", "provisional", "differential", "confirmed", "refuted", "entered-in-error")
	return exec(rec, `INSERT INTO conditions (id, raw_resource_id, patient_ref, code_system, disease_code, name, label,
			clinical_status, verification_status, onset_date)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, store.Nullable(patientRef), store.Nullable(code.System), store.Nullable(code.Code),
		store.Nullable(code.Display), store.Nullable(code.Text), store.Nullable(clinical), store.Nullable(verification), store.Nullable(onset))
}

// ---------- MedicationRequest ----------

func insertMedication(rec *recordCtx) error {
	r := rec.r
	patientRef, err := rec.patientRef()
	if err != nil {
		return err
	}
	var code fhir.Coding
	if _, ok := r["medicationReference"]; ok {
		if err := rec.issue(MissingCode, Warning, "medicationReference",
			"drug is a link to a Medication record, which is not supported yet", "drug code stored as null"); err != nil {
			return err
		}
	} else if code, err = rec.code("medicationCodeableConcept", "drug code"); err != nil {
		return err
	}
	prescribedAt, err := rec.date("authoredOn")
	if err != nil {
		return err
	}
	var dosage []string
	for _, d := range fhir.List(r, "dosageInstruction") {
		if t := fhir.Str(d, "text"); t != "" {
			dosage = append(dosage, t)
		}
	}
	status := allowed(fhir.Str(r, "status"), "active", "on-hold", "cancelled", "completed", "entered-in-error", "stopped", "draft", "unknown")
	intent := allowed(fhir.Str(r, "intent"), "proposal", "plan", "order", "original-order", "reflex-order", "filler-order", "instance-order", "option")
	return exec(rec, `INSERT INTO medications (id, raw_resource_id, patient_ref, code_system, drug_code, name, label,
			status, intent, prescribed_at, dosage)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, store.Nullable(patientRef), store.Nullable(code.System), store.Nullable(code.Code),
		store.Nullable(code.Display), store.Nullable(code.Text), store.Nullable(status), store.Nullable(intent),
		store.Nullable(prescribedAt), store.Nullable(strings.Join(dosage, "; ")))
}

// ---------- Procedure ----------

func insertProcedure(rec *recordCtx) error {
	r := rec.r
	patientRef, err := rec.patientRef()
	if err != nil {
		return err
	}
	code, err := rec.code("code", "procedure code")
	if err != nil {
		return err
	}
	performedAt, err := rec.date("performedDateTime")
	if err != nil {
		return err
	}
	if performedAt == "" {
		if performedAt, err = rec.date("performedPeriod", "start"); err != nil {
			return err
		}
	}
	performer := fhir.Str(r, "performer", 0, "actor", "reference")
	status := allowed(fhir.Str(r, "status"), "preparation", "in-progress", "not-done", "on-hold", "stopped", "completed", "entered-in-error", "unknown")
	return exec(rec, `INSERT INTO procedures (id, raw_resource_id, patient_ref, code_system, procedure_code, name, label,
			status, performed_at, performer)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, store.Nullable(patientRef), store.Nullable(code.System), store.Nullable(code.Code),
		store.Nullable(code.Display), store.Nullable(code.Text), store.Nullable(status), store.Nullable(performedAt), store.Nullable(performer))
}
