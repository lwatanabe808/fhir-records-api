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

// usableObservation is the filter for observations that research can trust.
const usableObservation = `o.is_primary = 1 AND o.replaced_by IS NULL AND o.is_impossible = 0
	AND coalesce(o.status, '') IN ('final', 'amended', 'corrected')`

// usableCondition leaves out diagnoses that were ruled out or entered by mistake.
const usableCondition = `coalesce(c.verification_status, '') NOT IN ('refuted', 'entered-in-error')`

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
	labMin, err := floatParam(r, "labMin")
	if paramError(w, err) {
		return
	}
	labMax, err := floatParam(r, "labMax")
	if paramError(w, err) {
		return
	}
	condition, medication, procedure, lab := q.Get("condition"), q.Get("medication"), q.Get("procedure"), labShortcut(q.Get("lab"))
	excludeCondition, excludeMedication, excludeProcedure := q.Get("excludeCondition"), q.Get("excludeMedication"), q.Get("excludeProcedure")
	if (labMin != nil || labMax != nil) && lab == "" {
		writeError(w, http.StatusBadRequest, "INVALID_PARAMETER", "labMin and labMax need lab, e.g. lab=a1c or lab=4548-4")
		return
	}
	if minAge > maxAge {
		writeError(w, http.StatusBadRequest, "INVALID_PARAMETER", "minAge is greater than maxAge")
		return
	}

	// 1. Patients matching the inclusion and exclusion criteria. Each criterion
	// matches a code exactly, or a name containing the text.
	where, args := []string{"1=1"}, []any{}
	if gender != "" {
		where = append(where, "p.gender = ?")
		args = append(args, gender)
	}
	add := func(value, matcher string, include bool) {
		if value == "" {
			return
		}
		op := "EXISTS"
		if !include {
			op = "NOT EXISTS"
		}
		where = append(where, op+" ("+matcher+")")
		like := "%" + strings.ToLower(value) + "%"
		args = append(args, value, like, like)
	}
	add(condition, conditionMatch, true)
	add(medication, medicationMatch, true)
	add(procedure, procedureMatch, true)
	add(excludeCondition, conditionMatch, false)
	add(excludeMedication, medicationMatch, false)
	add(excludeProcedure, procedureMatch, false)
	rows, err := s.db.Query(`SELECT p.id, p.gender, p.birth_date FROM patients p WHERE `+strings.Join(where, " AND ")+` ORDER BY p.id`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	type candidate struct {
		id     string
		gender sql.NullString
		birth  sql.NullString
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.gender, &c.birth); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		candidates = append(candidates, c)
	}
	rows.Close()

	// 2. Latest trustworthy lab value per patient, if a lab was asked for.
	var labs map[string]latestLab
	if lab != "" {
		if labs, err = s.latestLabs(lab); err != nil {
			serverError(w, err)
			return
		}
	}

	// 3. Age and lab filters.
	now := s.now()
	var matched []candidate
	for _, c := range candidates {
		if minAge > 0 || maxAge < 150 {
			age, ok := ageOn(c.birth.String, now)
			if !ok || age < minAge || age > maxAge {
				continue
			}
		}
		if lab != "" {
			l, ok := labs[c.id]
			if !ok || (labMin != nil && l.value < *labMin) || (labMax != nil && l.value > *labMax) {
				continue
			}
		}
		matched = append(matched, c)
	}

	// 4. The same person can come from two clinics under two patient ids. They
	// are never merged, but the count says how many might be duplicates.
	matchedIDs := make([]string, len(matched))
	for i, c := range matched {
		matchedIDs[i] = c.id
	}
	dupOf, extra, err := s.duplicateGroups(matchedIDs)
	if err != nil {
		serverError(w, err)
		return
	}

	// 5. De-identified details for this page.
	page := []map[string]any{}
	for i := offset; i < len(matched) && i < offset+limit; i++ {
		c := matched[i]
		conds, err := s.cohortConditions(c.id)
		if err != nil {
			serverError(w, err)
			return
		}
		meds, err := s.cohortMedications(c.id)
		if err != nil {
			serverError(w, err)
			return
		}
		p := map[string]any{"researchId": s.researchID(c.id), "age": deidentifiedAge(c.birth.String, now),
			"gender": nullStr(c.gender), "conditions": conds, "medications": meds}
		if lab != "" {
			l := labs[c.id]
			p["lab"] = map[string]any{"test": l.name, "code": lab, "value": l.value, "unit": l.unit, "year": yearOnly(l.date)}
		}
		if others := dupOf[c.id]; len(others) > 0 {
			ids := make([]string, len(others))
			for j, o := range others {
				ids[j] = s.researchID(o)
			}
			p["possibleDuplicateOf"] = ids
		}
		page = append(page, p)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"criteria": map[string]any{"condition": nullIfEmpty(condition), "medication": nullIfEmpty(medication),
			"procedure": nullIfEmpty(procedure), "lab": nullIfEmpty(lab), "labMin": labMin, "labMax": labMax,
			"minAge": minAge, "maxAge": maxAge, "gender": nullIfEmpty(gender),
			"excludeCondition": nullIfEmpty(excludeCondition), "excludeMedication": nullIfEmpty(excludeMedication),
			"excludeProcedure": nullIfEmpty(excludeProcedure)},
		// The conservative number: possible duplicate patients count once.
		"countWithoutDuplicates":           len(matched) - extra,
		"countIncludingPossibleDuplicates": len(matched),
		"possibleDuplicatePatients":        extra,
		"limit":                            limit,
		"offset":                           offset,
		"patients":                         page,
		"note":                             "De-identified: no names, ids or birth dates; ages over 89 shown as 90+; dates reduced to the year. Only usable records count.",
	})
}

// Matchers for inclusion and exclusion criteria. Each takes the value three
// times: an exact code, then a name and a label containing the text.
const (
	conditionMatch = `SELECT 1 FROM conditions c WHERE c.patient_id = p.id AND ` + usableCondition + `
		AND (c.disease_code = ? OR lower(c.name) LIKE ? OR lower(c.label) LIKE ?)`
	medicationMatch = `SELECT 1 FROM medications m WHERE m.patient_id = p.id
		AND coalesce(m.status, '') <> 'entered-in-error'
		AND (m.drug_code = ? OR lower(m.name) LIKE ? OR lower(m.label) LIKE ?)`
	procedureMatch = `SELECT 1 FROM procedures pr WHERE pr.patient_id = p.id
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

// duplicateGroups finds possible duplicates within a list of patient ids: the
// same name (ignoring case, accents and punctuation) and birth date.
//
// dupOf lists, for each id, the other ids in the list that may be the same
// person. extra is how many ids are extra (beyond the first in each group), so
// len(ids) - extra is the count without possible duplicates.
func (s *Server) duplicateGroups(ids []string) (dupOf map[string][]string, extra int, err error) {
	dupOf = map[string][]string{}
	if len(ids) < 2 {
		return dupOf, 0, nil
	}
	in := map[string]bool{}
	for _, id := range ids {
		in[id] = true
	}
	rows, err := s.db.Query(`SELECT a.id, b.id FROM patients a JOIN patients b
		ON a.id <> b.id AND a.name_key = b.name_key AND a.birth_date = b.birth_date
		ORDER BY 1, 2`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, 0, err
		}
		if in[a] && in[b] {
			dupOf[a] = append(dupOf[a], b)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for _, id := range ids {
		if isExtraDuplicate(id, dupOf) {
			extra++
		}
	}
	return dupOf, extra, nil
}

func (s *Server) latestLabs(code string) (map[string]latestLab, error) {
	// Keyed by patient: each patient's latest value.
	// observation_values holds single results and components alike, so a part
	// like systolic blood pressure can be searched by its own lab test code.
	rows, err := s.db.Query(`SELECT o.patient_id, v.value, coalesce(v.unit, ''), o.measured_at, coalesce(v.name, o.name, o.label, '')
		FROM observation_values v JOIN observations o ON o.id = v.observation_id
		WHERE o.patient_id IS NOT NULL AND v.lab_test_code = ? AND v.value IS NOT NULL AND `+usableObservation+`
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

func (s *Server) cohortConditions(patientID string) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT c.disease_code, coalesce(c.name, c.label), c.clinical_status, c.onset_date
		FROM conditions c WHERE c.patient_id = ? AND c.disease_code IS NOT NULL AND `+usableCondition+` ORDER BY c.onset_date`, patientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var code, name, status, onset sql.NullString
		if err := rows.Scan(&code, &name, &status, &onset); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"name": nullStr(name), "diseaseCode": nullStr(code), "status": nullStr(status), "onsetYear": yearOnly(onset)})
	}
	return out, rows.Err()
}

func (s *Server) cohortMedications(patientID string) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT drug_code, coalesce(name, label), status FROM medications
		WHERE patient_id = ? AND coalesce(status, '') <> 'entered-in-error' ORDER BY prescribed_at`, patientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var code, name, status sql.NullString
		if err := rows.Scan(&code, &name, &status); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"name": nullStr(name), "drugCode": nullStr(code), "status": nullStr(status)})
	}
	return out, rows.Err()
}

// isExtraDuplicate reports whether id is not the first (alphabetically) in its
// possible duplicate group.
func isExtraDuplicate(id string, dupOf map[string][]string) bool {
	for _, other := range dupOf[id] {
		if other < id {
			return true
		}
	}
	return false
}
