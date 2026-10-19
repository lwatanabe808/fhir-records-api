package api

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/lwatanabe808/novellia-fhir-api/internal/fhir"
)

// GET /research/dataset-profile
//
// Answers the first questions a research partner asks about a dataset before
// buying it: how long is each patient's history, how complete and well coded
// is it, how clean is it, and who is in it. All numbers are aggregates; any
// population group smaller than the minimum cell size is shown as "<N" so a
// tiny group can't single someone out.
func (s *Server) datasetProfile(w http.ResponseWriter, r *http.Request) {
	history, err := s.historyStats()
	if err != nil {
		serverError(w, err)
		return
	}
	completeness, err := s.percentages(map[string]string{
		"patientsWithBirthDate":       `SELECT avg(birth_date IS NOT NULL) FROM patients`,
		"patientsWithGender":          `SELECT avg(gender IS NOT NULL) FROM patients`,
		"recordsLinkedToPatient":      `SELECT avg(patient_id IS NOT NULL) FROM (SELECT patient_id FROM observations UNION ALL SELECT patient_id FROM conditions UNION ALL SELECT patient_id FROM medications UNION ALL SELECT patient_id FROM procedures UNION ALL SELECT patient_id FROM document_references)`,
		"observationsWithLabTestCode": `SELECT avg(lab_test_code IS NOT NULL) FROM observations`,
		"numericResultsWithUnit":      `SELECT avg(unit IS NOT NULL) FROM observations WHERE value IS NOT NULL`,
		"conditionsWithDiseaseCode":   `SELECT avg(disease_code IS NOT NULL) FROM conditions`,
		"medicationsWithDrugCode":     `SELECT avg(drug_code IS NOT NULL) FROM medications`,
		"medicationsWithDosage":       `SELECT avg(dosage IS NOT NULL) FROM medications`,
		"notesWithReadableText":       `SELECT avg(EXISTS (SELECT 1 FROM document_attachments a JOIN binaries b ON b.id = a.binary_id WHERE a.document_id = d.id AND b.text IS NOT NULL)) FROM document_references d`,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	coding, err := s.codingStandards()
	if err != nil {
		serverError(w, err)
		return
	}
	quality, err := s.percentages(map[string]string{
		"observationsImpossible":          `SELECT avg(is_impossible) FROM observations`,
		"observationsImplausible":         `SELECT avg(is_implausible) FROM observations`,
		"observationsDuplicateOrReplaced": `SELECT avg(is_primary = 0) FROM observations`,
		"linesThatFailedToLoad":           `SELECT avg(parsed_ok = 0 OR resource_type IS NULL) FROM raw_resources`,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	var possibleDupPatients int
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT record_id) FROM issues WHERE issue_type = 'POSSIBLE_DUPLICATE_PATIENT'`).Scan(&possibleDupPatients); err != nil {
		serverError(w, err)
		return
	}
	population, err := s.population()
	if err != nil {
		serverError(w, err)
		return
	}
	// One id per person: people confirmed by SSN count once.
	allIDs, err := s.stringList(`SELECT id FROM patients WHERE id = person_id ORDER BY id`)
	if err != nil {
		serverError(w, err)
		return
	}
	_, extra, err := s.duplicateGroups(allIDs)
	if err != nil {
		serverError(w, err)
		return
	}

	quality["patientsPossiblyDuplicated"] = possibleDupPatients
	writeJSON(w, http.StatusOK, map[string]any{
		"patientsWithoutDuplicates":           len(allIDs) - extra, // possible duplicates count once
		"patientsIncludingPossibleDuplicates": len(allIDs),
		"history":                             history,
		"completenessPercent":                 completeness,
		"codingStandards":                     coding,
		"quality":                             quality,
		"population":                          population,
		"minCellSize":                         s.minCellSize,
		"note":                                "Aggregates only. Population groups smaller than minCellSize are shown as \"<N\". Percentages are of all records loaded.",
	})
}

// historyStats measures how long and how full each patient's record is.
func (s *Server) historyStats() (map[string]any, error) {
	// Measured per person, so confirmed matches count as one history.
	rows, err := s.db.Query(`SELECT pt.person_id, x.d FROM (
		SELECT patient_id, measured_at AS d FROM observations WHERE patient_id IS NOT NULL AND measured_at IS NOT NULL
		UNION ALL SELECT patient_id, onset_date FROM conditions WHERE patient_id IS NOT NULL AND onset_date IS NOT NULL
		UNION ALL SELECT patient_id, prescribed_at FROM medications WHERE patient_id IS NOT NULL AND prescribed_at IS NOT NULL
		UNION ALL SELECT patient_id, performed_at FROM procedures WHERE patient_id IS NOT NULL AND performed_at IS NOT NULL
		UNION ALL SELECT patient_id, date FROM document_references WHERE patient_id IS NOT NULL AND date IS NOT NULL
		) x JOIN patients pt ON pt.id = x.patient_id`)
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

// percentages runs queries that return a 0 to 1 average and turns them into percents.
func (s *Server) percentages(queries map[string]string) (map[string]any, error) {
	out := map[string]any{}
	for key, q := range queries {
		var v sql.NullFloat64
		if err := s.db.QueryRow(q).Scan(&v); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if v.Valid {
			out[key] = math.Round(v.Float64*1000) / 10
		} else {
			out[key] = nil
		}
	}
	return out, nil
}

// codingStandards reports which code lists the codes come from. Standard
// lists (LOINC, SNOMED, RxNorm) can be compared across datasets; local clinic
// codes can't.
func (s *Server) codingStandards() (map[string]any, error) {
	rows, err := s.db.Query(`
		SELECT 'observations' AS t, code_system FROM observations WHERE lab_test_code IS NOT NULL
		UNION ALL SELECT 'conditions', code_system FROM conditions WHERE disease_code IS NOT NULL
		UNION ALL SELECT 'medications', code_system FROM medications WHERE drug_code IS NOT NULL
		UNION ALL SELECT 'procedures', code_system FROM procedures WHERE procedure_code IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]string{
		"http://loinc.org":                            "LOINC",
		"http://snomed.info/sct":                      "SNOMED",
		"http://www.nlm.nih.gov/research/umls/rxnorm": "RxNorm",
	}
	counts := map[string]map[string]int{}
	totals := map[string]int{}
	for rows.Next() {
		var table string
		var system sql.NullString
		if err := rows.Scan(&table, &system); err != nil {
			return nil, err
		}
		name := names[system.String]
		switch {
		case !system.Valid:
			name = "no code system"
		case name == "":
			name = "other"
		}
		if counts[table] == nil {
			counts[table] = map[string]int{}
		}
		counts[table][name]++
		totals[table]++
	}
	out := map[string]any{}
	for table, byName := range counts {
		pct := map[string]float64{}
		for name, n := range byName {
			pct[name] = math.Round(float64(n)/float64(totals[table])*1000) / 10
		}
		out[table] = pct
	}
	return out, rows.Err()
}

// population gives age bands and gender, with small groups hidden.
func (s *Server) population() (map[string]any, error) {
	rows, err := s.db.Query(`SELECT coalesce(gender, 'unknown'), birth_date FROM patients WHERE id = person_id`)
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
