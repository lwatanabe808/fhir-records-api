package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
)

// Endpoints for Novellia's data team: what was wrong in the data, and what
// the loader did about it. Problems are visible here instead of only in logs.

// ---------- GET /data-quality/summary ----------

func (s *Server) qualitySummary(w http.ResponseWriter, r *http.Request) {
	tables := map[string]string{
		"patients": "patients", "observations": "observations", "conditions": "conditions",
		"medications": "medications", "procedures": "procedures", "notes": "document_references", "binaries": "binaries",
	}
	clean := map[string]int{}
	for key, table := range tables {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			serverError(w, err)
			return
		}
		clean[key] = n
	}

	bySeverity := map[string]int{}
	byType := []map[string]any{}
	rows, err := s.db.Query(`SELECT issue_type, severity, COUNT(*) FROM issues GROUP BY issue_type, severity ORDER BY 3 DESC, 1`)
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
	completeness, err := s.percentages(map[string]string{
		"patientsWithBirthDate":       `SELECT avg(birth_date IS NOT NULL) FROM patients`,
		"patientsWithGender":          `SELECT avg(gender IS NOT NULL) FROM patients`,
		"observationsLinkedToPatient": `SELECT avg(patient_id IS NOT NULL) FROM observations`,
		"observationsWithCode":        `SELECT avg(lab_test_code IS NOT NULL) FROM observations`,
		"conditionsWithCode":          `SELECT avg(disease_code IS NOT NULL) FROM conditions`,
		"notesWithText":               `SELECT avg(EXISTS (SELECT 1 FROM document_attachments a JOIN binaries b ON b.id = a.binary_id WHERE a.document_id = d.id AND b.text IS NOT NULL)) FROM document_references d`,
	})
	if err != nil {
		serverError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"load":                s.summary,
		"cleanRecords":        clean,
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
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM issues i WHERE `+cond, args...).Scan(&total); err != nil {
		serverError(w, err)
		return
	}
	rows, err := s.db.Query(`SELECT i.id, i.raw_resource_id, i.record_type, i.record_id, i.issue_type, i.severity,
			i.field, i.detail, i.action_taken, i.group_key, coalesce(rr.raw_json, rr.raw_text)
		FROM issues i LEFT JOIN raw_resources rr ON rr.id = i.raw_resource_id
		WHERE `+cond+` ORDER BY i.raw_resource_id, i.id LIMIT ? OFFSET ?`, append(args, limit, offset)...)
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
	err := s.db.QueryRow(`SELECT raw_json FROM raw_resources WHERE resource_type = ? AND fhir_id = ? AND raw_json IS NOT NULL
		ORDER BY id DESC LIMIT 1`, r.PathValue("type"), r.PathValue("id")).Scan(&raw)
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

// ---------- GET /fhir/{type}/{id}/history ----------

// Every version of a record, in the order it appeared in the file. When the
// same type and id arrive more than once, the clean tables keep only the
// current version, so this is where the older versions can be seen.
func (s *Server) rawRecordHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT r.id, r.raw_json,
			(SELECT i.issue_type FROM issues i WHERE i.raw_resource_id = r.id
			 AND i.issue_type IN ('DUPLICATE_ID', 'DUPLICATE_ID_CHANGED') LIMIT 1)
		FROM raw_resources r WHERE r.resource_type = ? AND r.fhir_id = ? AND r.raw_json IS NOT NULL
		ORDER BY r.id`, r.PathValue("type"), r.PathValue("id"))
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type version struct {
		line  int64
		raw   string
		issue sql.NullString
	}
	var versions []version
	for rows.Next() {
		var v version
		if err := rows.Scan(&v.line, &v.raw, &v.issue); err != nil {
			serverError(w, err)
			return
		}
		versions = append(versions, v)
	}
	if len(versions) == 0 {
		writeError(w, http.StatusNotFound, "RECORD_NOT_FOUND", "no "+r.PathValue("type")+" with id "+r.PathValue("id"))
		return
	}

	// Which version is in the clean table: the one whose line it points to.
	var currentLine sql.NullInt64
	table := map[string]string{"Patient": "patients", "Observation": "observations", "Condition": "conditions",
		"MedicationRequest": "medications", "Procedure": "procedures", "DocumentReference": "document_references", "Binary": "binaries"}[r.PathValue("type")]
	if table != "" {
		_ = s.db.QueryRow(`SELECT raw_resource_id FROM `+table+` WHERE id = ?`, r.PathValue("id")).Scan(&currentLine)
	}

	out := []map[string]any{}
	for i, v := range versions {
		item := map[string]any{"version": i + 1, "line": v.line, "resource": json.RawMessage(redactRaw(v.raw))}
		switch {
		case currentLine.Valid && v.line == currentLine.Int64:
			item["status"] = "current"
		case v.issue.String == "DUPLICATE_ID":
			item["status"] = "identical copy, skipped"
		default:
			item["status"] = "replaced"
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"type": r.PathValue("type"), "id": r.PathValue("id"), "versions": out})
}
