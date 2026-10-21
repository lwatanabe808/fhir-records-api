package api

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"strings"
)

// Endpoints for Novellia's data team: what was wrong in the data, and what
// the loader did about it. Problems are visible here instead of only in logs.

// ---------- GET /data-quality/summary ----------

func (s *Server) qualitySummary(w http.ResponseWriter, r *http.Request) {
	// determine number of records by type, all in one query
	var patients, observations, conditions, medications, procedures, notes, binaries int
	err := s.db.QueryRow(`
SELECT
    (SELECT COUNT(*) FROM patients),
    (SELECT COUNT(*) FROM observations),
    (SELECT COUNT(*) FROM conditions),
    (SELECT COUNT(*) FROM medications),
    (SELECT COUNT(*) FROM procedures),
    (SELECT COUNT(*) FROM document_references),
    (SELECT COUNT(*) FROM binaries)`).Scan(&patients, &observations, &conditions, &medications, &procedures, &notes, &binaries)
	if err != nil {
		serverError(w, err)
		return
	}
	recordsByType := map[string]int{
		"patients": patients, "observations": observations, "conditions": conditions,
		"medications": medications, "procedures": procedures, "notes": notes, "binaries": binaries,
	}

	bySeverity := map[string]int{}
	byType := []map[string]any{}
	rows, err := s.db.Query(`
SELECT issue_type, severity, COUNT(*)
FROM issues
GROUP BY issue_type, severity
ORDER BY 3 DESC, 1`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var t, sev string
		var n int
		if err := rows.Scan(&t, &sev, &n); err != nil {
			serverError(w, err)
			return
		}
		bySeverity[sev] += n
		byType = append(byType, map[string]any{"type": t, "severity": sev, "count": n})
	}

	// Completeness: how often key fields are filled in. This is what research
	// partners ask about first ("how complete is your data?").
	// All in one query. avg() of a true/false check is the share that is true.
	var birthDate, gender, linked, labCode, diseaseCode, noteText sql.NullFloat64
	err = s.db.QueryRow(`
SELECT
    (SELECT avg(birth_date IS NOT NULL) FROM patients),
    (SELECT avg(gender IS NOT NULL) FROM patients),
    (SELECT avg(patient_id IS NOT NULL) FROM observations),
    (SELECT avg(lab_test_code IS NOT NULL) FROM observations),
    (SELECT avg(disease_code IS NOT NULL) FROM conditions),
    (SELECT avg(EXISTS (
        SELECT 1
        FROM document_attachments a
        JOIN binaries b ON b.id = a.binary_id
        WHERE a.document_id = d.id
        AND b.text IS NOT NULL
    )) FROM document_references d)`).Scan(&birthDate, &gender, &linked, &labCode, &diseaseCode, &noteText)
	if err != nil {
		serverError(w, err)
		return
	}
	completeness := map[string]any{
		"patientsWithBirthDate":       percent(birthDate),
		"patientsWithGender":          percent(gender),
		"observationsLinkedToPatient": percent(linked),
		"observationsWithCode":        percent(labCode),
		"conditionsWithCode":          percent(diseaseCode),
		"notesWithText":               percent(noteText),
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"load":                s.summary,
		"recordsByType":       recordsByType,
		"issuesBySeverity":    bySeverity,
		"issuesByType":        byType,
		"completenessPercent": completeness,
	})
}

// ---------- GET /data-quality/issues ----------

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", 100, 1, 1000)
	if paramError(w, err) {
		return
	}
	offset, err := intParam(r, "offset", 0, 0, 1_000_000)
	if paramError(w, err) {
		return
	}
	severity, err := enumParam(r, "severity", "error", "warning", "info")
	if paramError(w, err) {
		return
	}
	where, args := []string{"1=1"}, []any{}
	q := r.URL.Query()
	if t := q.Get("type"); t != "" {
		where = append(where, "i.issue_type = ?")
		args = append(args, strings.ToUpper(t))
	}
	if severity != "" {
		where = append(where, "i.severity = ?")
		args = append(args, severity)
	}
	if rt := q.Get("recordType"); rt != "" {
		where = append(where, "i.record_type = ?")
		args = append(args, rt)
	}
	if id := q.Get("recordId"); id != "" {
		where = append(where, "i.record_id = ?")
		args = append(args, id)
	}
	if g := q.Get("groupKey"); g != "" {
		where = append(where, "i.group_key = ?")
		args = append(args, g)
	}
	includeRaw, err := boolParam(r, "includeRaw")
	if paramError(w, err) {
		return
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow(`
SELECT COUNT(*)
FROM issues i
WHERE `+cond, args...).Scan(&total); err != nil {
		serverError(w, err)
		return
	}
	rows, err := s.db.Query(`
SELECT i.id, i.raw_resource_id, i.record_type, i.record_id, i.issue_type, i.severity, i.field, i.detail, i.action_taken, i.group_key, coalesce(rr.raw_json, rr.raw_text)
FROM issues i
LEFT JOIN raw_resources rr ON rr.id = i.raw_resource_id
WHERE `+cond+`
ORDER BY i.raw_resource_id, i.id
LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id int64
		var line sql.NullInt64
		var rt, rid, t, sev, field, detail, action, group, raw sql.NullString
		if err := rows.Scan(&id, &line, &rt, &rid, &t, &sev, &field, &detail, &action, &group, &raw); err != nil {
			serverError(w, err)
			return
		}
		item := map[string]any{"id": id, "type": t.String, "severity": sev.String, "recordType": nullStr(rt),
			"recordId": nullStr(rid), "field": nullStr(field), "detail": nullStr(detail), "actionTaken": nullStr(action),
			"groupKey": nullStr(group)}
		if line.Valid {
			item["line"] = line.Int64
		}
		if includeRaw && raw.Valid {
			if json.Valid([]byte(raw.String)) {
				item["raw"] = json.RawMessage(redactRaw(raw.String))
			} else {
				item["raw"] = raw.String
			}
		}
		list = append(list, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "limit": limit, "offset": offset, "issues": list})
}

// ---------- GET /fhir/{type}/{id} ----------

// The original record exactly as it arrived. If the same id appeared more
// than once, the last copy is returned.
func (s *Server) rawRecord(w http.ResponseWriter, r *http.Request) {
	var raw string
	err := s.db.QueryRow(`
SELECT raw_json
FROM raw_resources
WHERE resource_type = ?
AND fhir_id = ?
AND raw_json IS NOT NULL
ORDER BY id DESC
LIMIT 1`, r.PathValue("type"), r.PathValue("id")).Scan(&raw)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "RECORD_NOT_FOUND", "no "+r.PathValue("type")+" with id "+r.PathValue("id"))
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/fhir+json")
	_, _ = w.Write([]byte(redactRaw(raw)))
}

// percent turns a share like 0.957 into 95.7, or null when the table is empty.
func percent(share sql.NullFloat64) any {
	if !share.Valid {
		return nil
	}
	return math.Round(share.Float64*1000) / 10
}
