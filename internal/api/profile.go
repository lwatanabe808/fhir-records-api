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
	allIDs, err := s.stringList(`SELECT id FROM patients ORDER BY id`)
	if err != nil {
		serverError(w, err)
		return
	}
	_, extra, err := s.duplicateGroups(allIDs)
	if err != nil {
		serverError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"patientsWithoutDuplicates":           len(allIDs) - extra, // possible duplicates count once
		"patientsIncludingPossibleDuplicates": len(allIDs),
		"possibleDuplicatePatients":           extra,
		"history":                             history,
		"population":                          population,
		"minCellSize":                         s.minCellSize,
		"note":                                "Aggregates only. Population groups smaller than minCellSize are shown as \"<N\".",
	})
}

// historyStats measures how long and how full each patient's record is.
func (s *Server) historyStats() (map[string]any, error) {
	rows, err := s.db.Query(`SELECT x.patient_id, x.d FROM (
		SELECT patient_id, measured_at AS d FROM observations WHERE patient_id IS NOT NULL AND measured_at IS NOT NULL
		UNION ALL SELECT patient_id, onset_date FROM conditions WHERE patient_id IS NOT NULL AND onset_date IS NOT NULL
		UNION ALL SELECT patient_id, prescribed_at FROM medications WHERE patient_id IS NOT NULL AND prescribed_at IS NOT NULL
		UNION ALL SELECT patient_id, performed_at FROM procedures WHERE patient_id IS NOT NULL AND performed_at IS NOT NULL
		UNION ALL SELECT patient_id, date FROM document_references WHERE patient_id IS NOT NULL AND date IS NOT NULL
		) x`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type span struct {
		first, last time.Time
		records     int
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
			spans[pid] = &span{first: t, last: t, records: 1}
			continue
		}
		sp.records++
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
	var years, counts, sinceLast []float64
	for _, sp := range spans {
		years = append(years, sp.last.Sub(sp.first).Hours()/24/365.25)
		counts = append(counts, float64(sp.records))
		sinceLast = append(sinceLast, s.now().Sub(sp.last).Hours()/24/365.25)
	}
	return map[string]any{
		"patientsWithDatedRecords": len(spans),
		"yearsOfHistory":           summary(years),
		"datedRecordsPerPatient":   summary(counts),
		"yearsSinceLatestRecord":   summary(sinceLast),
	}, nil
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
	rows, err := s.db.Query(`SELECT coalesce(gender, 'unknown'), birth_date FROM patients`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bands := []struct {
		name     string
		min, max int
	}{{"0-17", 0, 17}, {"18-34", 18, 34}, {"35-49", 35, 49}, {"50-64", 50, 64}, {"65-79", 65, 79}, {"80+", 80, 200}}
	ages := map[string]int{}
	genders := map[string]int{}
	for rows.Next() {
		var gender string
		var birth sql.NullString
		if err := rows.Scan(&gender, &birth); err != nil {
			return nil, err
		}
		genders[gender]++
		age, ok := ageOn(birth.String, s.now()).(int)
		if !ok {
			ages["unknown"]++
			continue
		}
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
