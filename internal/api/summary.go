package api

import (
	"database/sql"
	"net/http"
)

// GET /patients/{id}/summary
//
// One call for the app's home screen: who the patient is, what's active now,
// and the latest result of each test. The full lists stay on their own endpoints.
func (s *Server) patientSummary(w http.ResponseWriter, r *http.Request) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return
	}
	var name, gender, birth sql.NullString
	if err := s.db.QueryRow(`SELECT full_name, gender, birth_date FROM patients WHERE id = ?`, id).Scan(&name, &gender, &birth); err != nil {
		serverError(w, err)
		return
	}

	conditions, err := s.rows(`SELECT id, coalesce(name, label, 'Unspecified condition'), onset_date FROM conditions
		WHERE `+ofPerson+` AND clinical_status IN ('active', 'recurrence', 'relapse')
		  AND coalesce(verification_status, '') NOT IN ('refuted', 'entered-in-error')
		ORDER BY onset_date DESC`, []string{"id", "condition", "onset"}, id)
	if err != nil {
		serverError(w, err)
		return
	}
	medications, err := s.rows(`SELECT id, coalesce(name, label), dosage, prescribed_at FROM medications
		WHERE `+ofPerson+` AND status = 'active' ORDER BY prescribed_at DESC`,
		[]string{"id", "medication", "dosage", "prescribedAt"}, id)
	if err != nil {
		serverError(w, err)
		return
	}
	// Latest visible result for each lab test code.
	labs, err := s.rows(`SELECT id, test, code, date FROM (
			SELECT id, coalesce(name, label) AS test, lab_test_code AS code, measured_at AS date,
			       ROW_NUMBER() OVER (PARTITION BY coalesce(code_system, ''), lab_test_code ORDER BY measured_at DESC, id) AS rn
			FROM observations WHERE `+ofPerson+` AND lab_test_code IS NOT NULL AND `+visibleObservation+`)
		WHERE rn = 1 ORDER BY test`, []string{"id", "test", "code", "date"}, id)
	if err != nil {
		serverError(w, err)
		return
	}
	for _, lab := range labs {
		if err := s.addValues(lab); err != nil {
			serverError(w, err)
			return
		}
	}
	procedures, err := s.rows(`SELECT id, coalesce(name, label), performed_at FROM procedures
		WHERE `+ofPerson+` AND coalesce(status, '') <> 'entered-in-error'
		ORDER BY performed_at DESC LIMIT 3`, []string{"id", "procedure", "performedAt"}, id)
	if err != nil {
		serverError(w, err)
		return
	}

	// How many problems were found in this patient's records.
	var issues int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM issues WHERE raw_resource_id IN (
			SELECT raw_resource_id FROM patients WHERE person_id = (SELECT person_id FROM patients WHERE id = ?)
			UNION SELECT raw_resource_id FROM observations WHERE `+ofPerson+`
			UNION SELECT raw_resource_id FROM conditions WHERE `+ofPerson+`
			UNION SELECT raw_resource_id FROM medications WHERE `+ofPerson+`
			UNION SELECT raw_resource_id FROM procedures WHERE `+ofPerson+`
			UNION SELECT raw_resource_id FROM document_references WHERE `+ofPerson+`)`,
		id, id, id, id, id, id).Scan(&issues); err != nil {
		serverError(w, err)
		return
	}
	var notes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM document_references WHERE `+ofPerson+` AND coalesce(status, '') <> 'entered-in-error'`, id).Scan(&notes); err != nil {
		serverError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"patient":            map[string]any{"id": id, "name": nullStr(name), "gender": nullStr(gender), "birthDate": nullStr(birth), "age": ageOn(birth.String, s.now())},
		"activeConditions":   conditions,
		"currentMedications": medications,
		"latestLabs":         labs,
		"recentProcedures":   procedures,
		"noteCount":          notes,
		"dataWarnings":       issues,
	})
}

// rows runs a query and returns each row as a map with the given keys.
// NULL values become JSON null.
func (s *Server) rows(query string, keys []string, args ...any) ([]map[string]any, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(keys))
		ptrs := make([]any, len(keys))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, k := range keys {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			m[k] = vals[i]
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
