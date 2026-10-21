package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Endpoints for life science partners (Novellia's B2B side).
//
// This is the "one additional capability": cohort search. Research partners
// ask questions like "patients with type 2 diabetes, age 40 to 60, on
// metformin, with an A1c over 7". This answers them with de-identified data:
//   - no names, ids or birth dates; a research id that can't be reversed
//   - ages over 89 shown as "90+", and dates reduced to the year
//     (both from the HIPAA Safe Harbor rules)
//   - only trustworthy records: no duplicates, corrections replaced, no
//     impossible values, no refuted or entered-in-error data

// researchID turns a patient id into a stable id that can't be turned back.
// HMAC with a secret key, so nobody can rebuild it by hashing known ids.
func (s *Server) researchID(patientID string) string {
	mac := hmac.New(sha256.New, s.patientIDSecret)
	mac.Write([]byte(patientID))
	return "r-" + hex.EncodeToString(mac.Sum(nil))[:12]
}

// deidentifiedAge returns the age, with everyone over 89 grouped as "90+".
func deidentifiedAge(birthDate string, now time.Time) any {
	age, ok := ageOn(birthDate, now)
	if !ok {
		return nil
	}
	if age > 89 {
		return "90+"
	}
	return age
}

func yearOnly(date sql.NullString) any {
	if !date.Valid || len(date.String) < 4 {
		return nil
	}
	return date.String[:4]
}

// ---------- GET /research/cohort ----------

type latestLab struct {
	value float64
	unit  string
	date  sql.NullString
	name  string
}

func (s *Server) cohort(w http.ResponseWriter, r *http.Request) {
	// get all parameters
	q := r.URL.Query()
	limit, err := intParam(r, "limit", 50, 1, 500)
	if paramError(w, err) {
		return
	}
	offset, err := intParam(r, "offset", 0, 0, 1_000_000)
	if paramError(w, err) {
		return
	}
	minAge, err := intParam(r, "minAge", 0, 0, 150)
	if paramError(w, err) {
		return
	}
	maxAge, err := intParam(r, "maxAge", 150, 0, 150)
	if paramError(w, err) {
		return
	}
	gender, err := enumParam(r, "gender", "male", "female", "other", "unknown")
	if paramError(w, err) {
		return
	}
	// minimum result
	labMin, err := floatParam(r, "labMin")
	if paramError(w, err) {
		return
	}
	// maximum result
	labMax, err := floatParam(r, "labMax")
	if paramError(w, err) {
		return
	}
	condition, medication, procedure, labCode := q.Get("condition"), q.Get("medication"), q.Get("procedure"), q.Get("lab")
	excludeCondition, excludeMedication, excludeProcedure := q.Get("excludeCondition"), q.Get("excludeMedication"), q.Get("excludeProcedure")
	if (labMin != nil || labMax != nil) && labCode == "" {
		writeError(w, http.StatusBadRequest, "INVALID_PARAMETER", "labMin and labMax need lab, e.g. lab=4548-4 (A1c)")
		return
	}
	if minAge > maxAge {
		writeError(w, http.StatusBadRequest, "INVALID_PARAMETER", "minAge is greater than maxAge")
		return
	}

	// 1. build where clauses for condition, medication, procedure (inclusion and exclusion)
	where, args := []string{"1=1"}, []any{}
	if gender != "" {
		where = append(where, "p.gender = ?")
		args = append(args, gender)
	}
	filters := []struct {
		value   string
		matcher string
		include bool
	}{
		{condition, conditionMatch, true},
		{medication, medicationMatch, true},
		{procedure, procedureMatch, true},
		{excludeCondition, conditionMatch, false},
		{excludeMedication, medicationMatch, false},
		{excludeProcedure, procedureMatch, false},
	}
	for _, f := range filters {
		if f.value == "" {
			continue // filter not given
		}
		if f.include {
			where = append(where, "EXISTS ("+f.matcher+")")
		} else {
			where = append(where, "NOT EXISTS ("+f.matcher+")")
		}
		like := "%" + strings.ToLower(f.value) + "%"
		// each query searches for code / name / label => value / like / like
		args = append(args, f.value, like, like)
	}
	// builds a name_birthdate_id value so that we can check if identical patients
	rows, err := s.db.Query(`
SELECT p.id, p.gender, p.birth_date, coalesce(p.name_key || '|' || p.birth_date, p.id)
FROM patients p
WHERE `+strings.Join(where, " AND ")+`
ORDER BY p.id`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	type candidate struct {
		id              string
		gender          sql.NullString
		birth           sql.NullString
		nameBirthdayKey string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.gender, &c.birth, &c.nameBirthdayKey); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		candidates = append(candidates, c)
	}
	rows.Close()

	// 2. Return latest lab for each patient based on labcode. We mostly care about right now.
	// We would want to limit this to only candidates in the future.
	var labs map[string]latestLab
	if labCode != "" {
		if labs, err = s.latestLabs(labCode); err != nil {
			serverError(w, err)
			return
		}
	}

	// 3. Check if age and lab results are outside of the min / max specified by user.
	now := s.now()
	var matched []candidate
	for _, c := range candidates {
		if q.Has("minAge") || q.Has("maxAge") {
			age, ok := ageOn(c.birth.String, now)
			if !ok || age < minAge || age > maxAge {
				continue
			}
		}
		if labCode != "" {
			l, ok := labs[c.id]
			if !ok || (labMin != nil && l.value < *labMin) || (labMax != nil && l.value > *labMax) {
				continue
			}
		}
		matched = append(matched, c)
	}

	// 4. The same person can come from two clinics under two patient ids.
	// We set up a nameBirthdayKey for each matched patient. We group the same nameBirthdayKeys together on the same key
	// After that, we can count the unique nameBirthdayKeys
	groups := map[string][]string{} // nameBirthdayKey => patient ids
	for _, c := range matched {
		groups[c.nameBirthdayKey] = append(groups[c.nameBirthdayKey], c.id)
	}

	// 5. De-identified details for this page.
	// One query for the conditions and one for the medications of every patient on the page.
	var pageIDs []any
	for i := offset; i < len(matched) && i < offset+limit; i++ {
		pageIDs = append(pageIDs, matched[i].id)
	}
	conds, err := s.cohortConditions(pageIDs)
	if err != nil {
		serverError(w, err)
		return
	}
	meds, err := s.cohortMedications(pageIDs)
	if err != nil {
		serverError(w, err)
		return
	}
	page := []map[string]any{}
	for i := offset; i < len(matched) && i < offset+limit; i++ {
		c := matched[i]
		p := map[string]any{"researchId": s.researchID(c.id), "age": deidentifiedAge(c.birth.String, now),
			"gender": nullStr(c.gender), "conditions": orEmpty(conds[c.id]), "medications": orEmpty(meds[c.id])}
		if labCode != "" {
			l := labs[c.id]
			p["lab"] = map[string]any{"test": l.name, "code": labCode, "value": l.value, "unit": l.unit, "year": yearOnly(l.date)}
		}
		var others []string
		for _, id := range groups[c.nameBirthdayKey] {
			if id != c.id {
				others = append(others, s.researchID(id))
			}
		}
		if len(others) > 0 {
			p["possibleDuplicateOf"] = others
		}
		page = append(page, p)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"criteria": map[string]any{"condition": nullIfEmpty(condition), "medication": nullIfEmpty(medication),
			"procedure": nullIfEmpty(procedure), "lab": nullIfEmpty(labCode), "labMin": labMin, "labMax": labMax,
			"minAge": minAge, "maxAge": maxAge, "gender": nullIfEmpty(gender),
			"excludeCondition": nullIfEmpty(excludeCondition), "excludeMedication": nullIfEmpty(excludeMedication),
			"excludeProcedure": nullIfEmpty(excludeProcedure)},
		// The conservative number: possible duplicate patients count once.
		"countWithoutDuplicates":           len(groups),
		"countIncludingPossibleDuplicates": len(matched),
		"possibleDuplicatePatients":        len(matched) - len(groups),
		"limit":                            limit,
		"offset":                           offset,
		"patients":                         page,
	})
}

// matches on disease_code, name, or label.
// excludes matches that were refuted or entered in error
const (
	conditionMatch = `
SELECT 1
FROM conditions c
WHERE c.patient_id = p.id
AND coalesce(c.verification_status, '') NOT IN ('refuted', 'entered-in-error')
AND (c.disease_code = ? OR lower(c.name) LIKE ? OR lower(c.label) LIKE ?)`
	medicationMatch = `
SELECT 1
FROM medications m
WHERE m.patient_id = p.id
AND coalesce(m.status, '') <> 'entered-in-error'
AND (m.drug_code = ? OR lower(m.name) LIKE ? OR lower(m.label) LIKE ?)`
	procedureMatch = `
SELECT 1
FROM procedures pr
WHERE pr.patient_id = p.id
AND coalesce(pr.status, '') NOT IN ('entered-in-error', 'not-done')
AND (pr.procedure_code = ? OR lower(pr.name) LIKE ? OR lower(pr.label) LIKE ?)`
)

// cell hides a count below the minimum cell size, so a small group can't
// single someone out.
func (s *Server) cell(n int) any {
	if n > 0 && n < s.minCellSize {
		return fmt.Sprintf("<%d", s.minCellSize)
	}
	return n
}

func (s *Server) latestLabs(code string) (map[string]latestLab, error) {
	// Keyed by patient: each patient's latest value.
	// observation_values holds single results and components alike, so a part
	// like systolic blood pressure can be searched by its own lab test code.
	rows, err := s.db.Query(`
SELECT o.patient_id, v.value, coalesce(v.unit, ''), o.measured_at, coalesce(v.name, o.name, o.label, '')
FROM observation_values v
JOIN observations o ON o.id = v.observation_id
WHERE o.patient_id IS NOT NULL
AND v.lab_test_code = ?
AND v.value IS NOT NULL
AND o.is_primary = 1
AND o.replaced_by IS NULL
AND o.is_impossible = 0
AND coalesce(o.status, '') IN ('final', 'amended', 'corrected')
ORDER BY o.patient_id, o.measured_at DESC`, code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]latestLab{}
	for rows.Next() {
		var pid string
		var l latestLab
		if err := rows.Scan(&pid, &l.value, &l.unit, &l.date, &l.name); err != nil {
			return nil, err
		}
		if _, seen := out[pid]; !seen { // rows are newest first, so keep the first one
			out[pid] = l
		}
	}
	return out, rows.Err()
}

// cohortConditions returns each patient's conditions, keyed by patient id.
func (s *Server) cohortConditions(patientIDs []any) (map[string][]map[string]any, error) {
	out := map[string][]map[string]any{}
	if len(patientIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`
SELECT c.patient_id, c.disease_code, coalesce(c.name, c.label), c.clinical_status, c.onset_date
FROM conditions c
WHERE c.patient_id IN (`+placeholders(len(patientIDs))+`)
AND c.disease_code IS NOT NULL
AND coalesce(c.verification_status, '') NOT IN ('refuted', 'entered-in-error')
ORDER BY c.onset_date`, patientIDs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var patientID string
		var code, name, status, onset sql.NullString
		if err := rows.Scan(&patientID, &code, &name, &status, &onset); err != nil {
			return nil, err
		}
		out[patientID] = append(out[patientID], map[string]any{"name": nullStr(name), "diseaseCode": nullStr(code), "status": nullStr(status), "onsetYear": yearOnly(onset)})
	}
	return out, rows.Err()
}

// cohortMedications returns each patient's medications, keyed by patient id.
func (s *Server) cohortMedications(patientIDs []any) (map[string][]map[string]any, error) {
	out := map[string][]map[string]any{}
	if len(patientIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`
SELECT patient_id, drug_code, coalesce(name, label), status
FROM medications
WHERE patient_id IN (`+placeholders(len(patientIDs))+`)
AND coalesce(status, '') <> 'entered-in-error'
ORDER BY prescribed_at`, patientIDs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var patientID string
		var code, name, status sql.NullString
		if err := rows.Scan(&patientID, &code, &name, &status); err != nil {
			return nil, err
		}
		out[patientID] = append(out[patientID], map[string]any{"name": nullStr(name), "drugCode": nullStr(code), "status": nullStr(status)})
	}
	return out, rows.Err()
}

// orEmpty turns a missing list into [] so the JSON shows an empty list, not null.
func orEmpty(list []map[string]any) []map[string]any {
	if list == nil {
		return []map[string]any{}
	}
	return list
}
