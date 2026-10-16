package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"sort"
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
//     impossible or implausible values, no refuted or entered-in-error data

// usableObservation is the filter for observations that research can trust.
const usableObservation = `o.is_primary = 1 AND o.replaced_by IS NULL AND o.is_impossible = 0 AND o.is_implausible = 0
	AND coalesce(o.status, '') IN ('final', 'amended', 'corrected')`

// usableCondition leaves out diagnoses that were ruled out or entered by mistake.
const usableCondition = `coalesce(c.verification_status, '') NOT IN ('refuted', 'entered-in-error')`

// researchID turns a patient id into a stable id that can't be turned back.
// HMAC with a secret key, so nobody can rebuild it by hashing known ids.
func (s *Server) researchID(patientID string) string {
	mac := hmac.New(sha256.New, s.idSecret)
	mac.Write([]byte(patientID))
	return "r-" + hex.EncodeToString(mac.Sum(nil))[:12]
}

// deidentifiedAge returns the age, with everyone over 89 grouped as "90+".
func deidentifiedAge(birthDate string, now time.Time) any {
	age, ok := ageOn(birthDate, now).(int)
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
	condition, medication, lab := q.Get("condition"), q.Get("medication"), labShortcut(q.Get("lab"))
	if (labMin != nil || labMax != nil) && lab == "" {
		writeError(w, http.StatusBadRequest, "INVALID_PARAMETER", "labMin and labMax need lab, e.g. lab=a1c or lab=4548-4")
		return
	}
	if minAge > maxAge {
		writeError(w, http.StatusBadRequest, "INVALID_PARAMETER", "minAge is greater than maxAge")
		return
	}

	// 1. Patients matching the condition, medication and gender filters.
	where, args := []string{"1=1"}, []any{}
	if gender != "" {
		where = append(where, "p.gender = ?")
		args = append(args, gender)
	}
	if condition != "" {
		where = append(where, `EXISTS (SELECT 1 FROM conditions c WHERE c.patient_id = p.id AND `+usableCondition+`
			AND (c.disease_code = ? OR lower(c.name) LIKE ? OR lower(c.label) LIKE ?))`)
		like := "%" + strings.ToLower(condition) + "%"
		args = append(args, condition, like, like)
	}
	if medication != "" {
		where = append(where, `EXISTS (SELECT 1 FROM medications m WHERE m.patient_id = p.id
			AND coalesce(m.status, '') <> 'entered-in-error'
			AND (m.drug_code = ? OR lower(m.name) LIKE ? OR lower(m.label) LIKE ?))`)
		like := "%" + strings.ToLower(medication) + "%"
		args = append(args, medication, like, like)
	}
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
			age, ok := ageOn(c.birth.String, now).(int)
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

	// 4. De-identified details for this page.
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
		page = append(page, p)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"criteria": map[string]any{"condition": nullIfEmpty(condition), "medication": nullIfEmpty(medication),
			"lab": nullIfEmpty(lab), "labMin": labMin, "labMax": labMax, "minAge": minAge, "maxAge": maxAge, "gender": nullIfEmpty(gender)},
		"count":    len(matched),
		"limit":    limit,
		"offset":   offset,
		"patients": page,
		"note":     "De-identified: no names, ids or birth dates; ages over 89 shown as 90+; dates reduced to the year. Only usable records count.",
	})
}

func (s *Server) latestLabs(code string) (map[string]latestLab, error) {
	rows, err := s.db.Query(`SELECT o.patient_id, o.value, coalesce(o.unit, ''), o.measured_at, coalesce(o.name, o.label, '')
		FROM observations o
		WHERE o.lab_test_code = ? AND o.patient_id IS NOT NULL AND o.value IS NOT NULL AND `+usableObservation+`
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

// ---------- GET /research/conditions ----------

// How many patients have each condition. Useful for the first question
// partners ask: "how many patients like this do you have?"
func (s *Server) conditionCounts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT c.code_system, c.disease_code, max(coalesce(c.name, c.label)), COUNT(DISTINCT c.patient_id) AS n
		FROM conditions c
		WHERE c.patient_id IS NOT NULL AND c.disease_code IS NOT NULL AND ` + usableCondition + `
		GROUP BY c.code_system, c.disease_code ORDER BY n DESC, 3`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var system, code, name sql.NullString
		var n int
		if err := rows.Scan(&system, &code, &name, &n); err != nil {
			serverError(w, err)
			return
		}
		out = append(out, map[string]any{"condition": nullStr(name), "codeSystem": nullStr(system), "diseaseCode": nullStr(code), "patients": n})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- GET /research/labs/{code} ----------

// Summary of each patient's latest value for one lab test. It reports the
// median and range, not the mean: a few outliers would pull a mean off.
func (s *Server) labSummary(w http.ResponseWriter, r *http.Request) {
	code := labShortcut(r.PathValue("code"))
	labs, err := s.latestLabs(code)
	if err != nil {
		serverError(w, err)
		return
	}
	byUnit := map[string][]float64{}
	name := ""
	for _, l := range labs {
		byUnit[l.unit] = append(byUnit[l.unit], l.value)
		name = l.name
	}
	units := []map[string]any{}
	for unit, values := range byUnit { // values in different units are never mixed
		sort.Float64s(values)
		units = append(units, map[string]any{"unit": unit, "patients": len(values), "min": values[0],
			"median": median(values), "max": values[len(values)-1]})
	}
	sort.Slice(units, func(i, j int) bool { return units[i]["patients"].(int) > units[j]["patients"].(int) })
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "test": nullIfEmpty(name), "patients": len(labs), "byUnit": units})
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
