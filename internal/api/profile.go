package api

import (
	"database/sql"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/lwatanabe808/novellia-fhir-api/internal/fhir"
)

// GET /research/dataset-profile
//
// What a research partner asks before buying data: how many patients, how long
// and how full each patient's history is, and who is in it. This matches what
// Novellia sells: long, complete patient histories. All numbers are aggregates;
// any population group smaller than the minimum cell size is shown as "<N".
func (s *Server) datasetProfile(w http.ResponseWriter, r *http.Request) {
	history, err := s.historyStats()
	if err != nil {
		serverError(w, err)
		return
	}
	population, err := s.population()
	if err != nil {
		serverError(w, err)
		return
	}
	// Patients with the same name and birth date count once.
	// Patients missing either one get their own id, so they never match anyone.
	var all, withoutDuplicates int
	err = s.db.QueryRow(`
SELECT COUNT(*), COUNT(DISTINCT coalesce(name_key || '|' || birth_date, id))
FROM patients`).Scan(&all, &withoutDuplicates)
	if err != nil {
		serverError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"patientsWithoutDuplicates":           withoutDuplicates,
		"patientsIncludingPossibleDuplicates": all,
		"possibleDuplicatePatients":           all - withoutDuplicates,
		"history":                             history,
		"population":                          population,
	})
}

// Union All stacks all thre results based on common columns
func (s *Server) historyStats() (map[string]any, error) {
	rows, err := s.db.Query(`
SELECT x.patient_id, x.d
FROM (
    SELECT patient_id, measured_at AS d
    FROM observations
    WHERE patient_id IS NOT NULL
    AND measured_at IS NOT NULL
    AND is_primary = 1
    AND replaced_by IS NULL
    AND is_impossible = 0
    AND coalesce(status, '') IN ('final', 'amended', 'corrected')
    UNION ALL
    SELECT patient_id, onset_date
    FROM conditions
    WHERE patient_id IS NOT NULL
    AND onset_date IS NOT NULL
    AND coalesce(verification_status, '') NOT IN ('refuted', 'entered-in-error')
    UNION ALL
    SELECT patient_id, prescribed_at
    FROM medications
    WHERE patient_id IS NOT NULL
    AND prescribed_at IS NOT NULL
    AND coalesce(status, '') <> 'entered-in-error'
    UNION ALL
    SELECT patient_id, performed_at
    FROM procedures
    WHERE patient_id IS NOT NULL
    AND performed_at IS NOT NULL
    AND coalesce(status, '') NOT IN ('entered-in-error', 'not-done')
    UNION ALL
    SELECT patient_id, date
    FROM document_references
    WHERE patient_id IS NOT NULL
    AND date IS NOT NULL
    AND coalesce(status, '') <> 'entered-in-error'
) x`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type span struct {
		first, last time.Time
	}
	spans := map[string]*span{}
	for rows.Next() {
		var pid, date string
		if err := rows.Scan(&pid, &date); err != nil {
			return nil, err
		}
		_, t, ok := fhir.ParseDate(date)
		if !ok || t.After(s.now()) {
			continue
		}
		sp := spans[pid]
		if sp == nil {
			spans[pid] = &span{first: t, last: t}
			continue
		}
		if t.Before(sp.first) {
			sp.first = t
		}
		if t.After(sp.last) {
			sp.last = t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var years, sinceLast []float64
	for _, sp := range spans {
		years = append(years, sp.last.Sub(sp.first).Hours()/24/365.25)
		sinceLast = append(sinceLast, s.now().Sub(sp.last).Hours()/24/365.25)
	}
	counts, err := s.recordsPerPatient()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"recordsPerPatient":        summary(counts),
		"patientsWithDatedRecords": len(spans), // how many patients the two date numbers below are based on
		"yearsOfHistory":           summary(years),
		"yearsSinceLatestRecord":   summary(sinceLast),
	}, nil
}

// recordsPerPatient counts each patient's records, dated or not. It uses the same
// rules as the cohort search, so duplicates, replaced results and mistakes don't count.
func (s *Server) recordsPerPatient() ([]float64, error) {
	rows, err := s.db.Query(`
SELECT
    (SELECT COUNT(*) FROM observations WHERE patient_id = p.id
        AND is_primary = 1 AND replaced_by IS NULL AND is_impossible = 0
        AND coalesce(status, '') IN ('final', 'amended', 'corrected')) +
    (SELECT COUNT(*) FROM conditions WHERE patient_id = p.id
        AND coalesce(verification_status, '') NOT IN ('refuted', 'entered-in-error')) +
    (SELECT COUNT(*) FROM medications WHERE patient_id = p.id
        AND coalesce(status, '') <> 'entered-in-error') +
    (SELECT COUNT(*) FROM procedures WHERE patient_id = p.id
        AND coalesce(status, '') NOT IN ('entered-in-error', 'not-done')) +
    (SELECT COUNT(*) FROM document_references WHERE patient_id = p.id
        AND coalesce(status, '') <> 'entered-in-error')
FROM patients p`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var counts []float64
	for rows.Next() {
		var n float64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		counts = append(counts, n)
	}
	return counts, rows.Err()
}

// summary reports median and range, not the mean: a few outliers would pull a mean off.
func summary(values []float64) map[string]any {
	if len(values) == 0 {
		return map[string]any{"median": nil, "min": nil, "max": nil}
	}
	sort.Float64s(values)
	round := func(f float64) float64 { return math.Round(f*10) / 10 }
	return map[string]any{"median": round(median(values)), "min": round(values[0]), "max": round(values[len(values)-1])}
}

// population gives age bands and gender, with small groups hidden.
func (s *Server) population() (map[string]any, error) {
	rows, err := s.db.Query(`
SELECT coalesce(gender, 'unknown'), birth_date
FROM patients`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ages := map[string]int{}
	genders := map[string]int{}
	for rows.Next() {
		var gender string
		var birth sql.NullString
		if err := rows.Scan(&gender, &birth); err != nil {
			return nil, err
		}
		genders[gender]++
		age, ok := ageOn(birth.String, s.now())
		if !ok {
			ages["unknown"]++
			continue
		}
		// store person in age band
		bands := []struct {
			name     string
			min, max int
		}{{"0-17", 0, 17}, {"18-34", 18, 34}, {"35-49", 35, 49}, {"50-64", 50, 64}, {"65-79", 65, 79}, {"80+", 80, 200}}
		for _, b := range bands {
			if age >= b.min && age <= b.max {
				ages[b.name]++
				break
			}
		}
	}
	return map[string]any{"ageBands": s.suppress(ages), "gender": s.suppress(genders)}, rows.Err()
}

// suppress hides each count below the minimum cell size (see cell).
func (s *Server) suppress(counts map[string]int) map[string]any {
	out := map[string]any{}
	for k, n := range counts {
		out[k] = s.cell(n)
	}
	return out
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
