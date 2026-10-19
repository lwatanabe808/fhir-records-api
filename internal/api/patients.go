package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Endpoints for the patient app: one person looking at their own records.
// Default views hide records that shouldn't count as real data (entered in
// error, replaced by a correction, duplicates, impossible values).
// ?includeHidden=true shows everything, with the reason it would be hidden.

// resolvePatient finds the patient for {id}. Ids work in any capitalization,
// as long as only one patient matches. It writes a 404 and returns "" otherwise.
func (s *Server) resolvePatient(w http.ResponseWriter, r *http.Request) string {
	id := r.PathValue("id")
	var found string
	err := s.db.QueryRow(`SELECT id FROM patients WHERE id = ?`, id).Scan(&found)
	if err == nil {
		return found
	}
	if err != sql.ErrNoRows {
		serverError(w, err)
		return ""
	}
	rows, err := s.db.Query(`SELECT id FROM patients WHERE lower(id) = lower(?)`, id)
	if err != nil {
		serverError(w, err)
		return ""
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			serverError(w, err)
			return ""
		}
		matches = append(matches, m)
	}
	if len(matches) == 1 {
		return matches[0]
	}
	writeError(w, http.StatusNotFound, "PATIENT_NOT_FOUND", "no patient with id "+id)
	return ""
}

type warning struct {
	Type   string `json:"type"`
	Detail string `json:"detail,omitempty"`
}

// warningsFor returns the issues logged for each raw line, so responses can
// say what was wrong with a record.
func (s *Server) warningsFor(rawIDs []int64) (map[int64][]warning, error) {
	out := map[int64][]warning{}
	if len(rawIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rawIDs)), ",")
	args := make([]any, len(rawIDs))
	for i, id := range rawIDs {
		args[i] = id
	}
	rows, err := s.db.Query(`SELECT raw_resource_id, issue_type, coalesce(detail, '') FROM issues
		WHERE raw_resource_id IN (`+ph+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var w warning
		if err := rows.Scan(&id, &w.Type, &w.Detail); err != nil {
			return nil, err
		}
		out[id] = append(out[id], w)
	}
	return out, rows.Err()
}

func emptyIfNil(w []warning) []warning {
	if w == nil {
		return []warning{}
	}
	return w
}

func ageOn(birthDate string, now time.Time) any {
	b, err := time.Parse("2006-01-02", birthDate)
	if err != nil {
		return nil
	}
	age := now.Year() - b.Year()
	if now.YearDay() < b.YearDay() {
		age--
	}
	return age
}

// ---------- GET /patients ----------

func (s *Server) listPatients(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", 50, 1, 500)
	if paramError(w, err) {
		return
	}
	offset, err := intParam(r, "offset", 0, 0, 1_000_000)
	if paramError(w, err) {
		return
	}
	gender, err := enumParam(r, "gender", "male", "female", "other", "unknown")
	if paramError(w, err) {
		return
	}
	bornAfter, err := dateParam(r, "bornAfter")
	if paramError(w, err) {
		return
	}
	bornBefore, err := dateParam(r, "bornBefore")
	if paramError(w, err) {
		return
	}

	where, args := []string{"1=1"}, []any{}
	if name := r.URL.Query().Get("name"); name != "" {
		where = append(where, "lower(full_name) LIKE ?")
		args = append(args, "%"+strings.ToLower(name)+"%")
	}
	if gender != "" {
		where = append(where, "gender = ?")
		args = append(args, gender)
	}
	if bornAfter != "" {
		where = append(where, "birth_date >= ?")
		args = append(args, bornAfter)
	}
	if bornBefore != "" {
		where = append(where, "birth_date <= ?")
		args = append(args, bornBefore)
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM patients WHERE `+cond, args...).Scan(&total); err != nil {
		serverError(w, err)
		return
	}
	rows, err := s.db.Query(`SELECT id, full_name, gender, birth_date FROM patients WHERE `+cond+
		` ORDER BY family_name, given_names, id LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id string
		var name, g, birth sql.NullString
		if err := rows.Scan(&id, &name, &g, &birth); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, map[string]any{"id": id, "name": nullStr(name), "gender": nullStr(g),
			"birthDate": nullStr(birth), "age": ageOn(birth.String, s.now())})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "limit": limit, "offset": offset, "patients": list})
}

// ---------- GET /patients/{id} ----------

func (s *Server) getPatient(w http.ResponseWriter, r *http.Request) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return
	}
	var raw int64
	var name, family, given, gender, birth sql.NullString
	var active sql.NullBool
	err := s.db.QueryRow(`SELECT raw_resource_id, full_name, family_name, given_names, gender, birth_date, active
		FROM patients WHERE id = ?`, id).Scan(&raw, &name, &family, &given, &gender, &birth, &active)
	if err != nil {
		serverError(w, err)
		return
	}
	warns, err := s.warningsFor([]int64{raw})
	if err != nil {
		serverError(w, err)
		return
	}
	counts := map[string]int{}
	for key, table := range map[string]string{"conditions": "conditions", "medications": "medications",
		"observations": "observations", "procedures": "procedures", "notes": "document_references"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?))`, id).Scan(&n); err != nil {
			serverError(w, err)
			return
		}
		counts[key] = n
	}
	var activeV any
	if active.Valid {
		activeV = active.Bool
	}
	// Other patient records confirmed as the same person (same SSN, name and
	// birth date). Their records are shown on this chart too.
	linked := []string{}
	lrows, err := s.db.Query(`SELECT id FROM patients WHERE person_id = (SELECT person_id FROM patients WHERE id = ?) AND id <> ? ORDER BY id`, id, id)
	if err != nil {
		serverError(w, err)
		return
	}
	for lrows.Next() {
		var l string
		if err := lrows.Scan(&l); err != nil {
			lrows.Close()
			serverError(w, err)
			return
		}
		linked = append(linked, l)
	}
	lrows.Close()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": nullStr(name), "familyName": nullStr(family), "givenNames": nullStr(given),
		"gender": nullStr(gender), "birthDate": nullStr(birth), "age": ageOn(birth.String, s.now()),
		"active": activeV, "linkedPatientIds": linked, "recordCounts": counts, "warnings": emptyIfNil(warns[raw]),
	})
}

// ---------- GET /patients/{id}/conditions ----------

func (s *Server) patientConditions(w http.ResponseWriter, r *http.Request) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return
	}
	status, err := enumParam(r, "status", "active", "recurrence", "relapse", "inactive", "remission", "resolved")
	if paramError(w, err) {
		return
	}
	includeHidden, err := boolParam(r, "includeHidden")
	if paramError(w, err) {
		return
	}
	// GET .../{recordId} returns one record, even if it would be hidden.
	single := r.PathValue("recordId") != ""
	includeHidden = includeHidden || single
	q := `SELECT id, raw_resource_id, code_system, disease_code, name, label, clinical_status, verification_status, onset_date
		FROM conditions WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?))`
	args := []any{id}
	if status != "" {
		q += ` AND clinical_status = ?`
		args = append(args, status)
	}
	if single {
		q += ` AND id = ?`
		args = append(args, r.PathValue("recordId"))
	}
	if !includeHidden {
		q += ` AND coalesce(verification_status, '') <> 'entered-in-error'`
	}
	rows, err := s.db.Query(q+` ORDER BY onset_date DESC, id`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	var items []map[string]any
	var raws []int64
	for rows.Next() {
		var cid string
		var raw int64
		var system, code, name, label, clinical, verification, onset sql.NullString
		if err := rows.Scan(&cid, &raw, &system, &code, &name, &label, &clinical, &verification, &onset); err != nil {
			serverError(w, err)
			return
		}
		display := name.String
		if display == "" {
			display = label.String
		}
		if display == "" {
			display = "Unspecified condition"
		}
		raws = append(raws, raw)
		items = append(items, map[string]any{"id": cid, "condition": display, "label": nullStr(label),
			"codeSystem": nullStr(system), "diseaseCode": nullStr(code), "status": nullStr(clinical),
			"verification": nullStr(verification), "onset": nullStr(onset), "_raw": raw})
	}
	s.respondWithWarnings(w, items, raws, single)
}

// respondWithWarnings attaches the issue list to each item and writes the
// response: the whole list, or one record when single is true.
func (s *Server) respondWithWarnings(w http.ResponseWriter, items []map[string]any, raws []int64, single bool) {
	warns, err := s.warningsFor(raws)
	if err != nil {
		serverError(w, err)
		return
	}
	for _, it := range items {
		raw := it["_raw"].(int64)
		delete(it, "_raw")
		it["warnings"] = emptyIfNil(warns[raw])
	}
	if single {
		if len(items) == 0 {
			writeError(w, http.StatusNotFound, "RECORD_NOT_FOUND", "this patient has no such record")
			return
		}
		writeJSON(w, http.StatusOK, items[0])
		return
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, items)
}

// ---------- GET /patients/{id}/medications ----------

func (s *Server) patientMedications(w http.ResponseWriter, r *http.Request) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return
	}
	status, err := enumParam(r, "status", "active", "on-hold", "cancelled", "completed", "stopped", "draft", "unknown")
	if paramError(w, err) {
		return
	}
	includeHidden, err := boolParam(r, "includeHidden")
	if paramError(w, err) {
		return
	}
	// GET .../{recordId} returns one record, even if it would be hidden.
	single := r.PathValue("recordId") != ""
	includeHidden = includeHidden || single
	q := `SELECT id, raw_resource_id, code_system, drug_code, name, label, status, prescribed_at, dosage
		FROM medications WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?))`
	args := []any{id}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if single {
		q += ` AND id = ?`
		args = append(args, r.PathValue("recordId"))
	}
	if !includeHidden {
		q += ` AND coalesce(status, '') <> 'entered-in-error'`
	}
	rows, err := s.db.Query(q+` ORDER BY prescribed_at DESC, id`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	var items []map[string]any
	var raws []int64
	for rows.Next() {
		var mid string
		var raw int64
		var system, code, name, label, st, at, dosage sql.NullString
		if err := rows.Scan(&mid, &raw, &system, &code, &name, &label, &st, &at, &dosage); err != nil {
			serverError(w, err)
			return
		}
		display := name.String
		if display == "" {
			display = label.String
		}
		raws = append(raws, raw)
		items = append(items, map[string]any{"id": mid, "medication": nullIfEmpty(display), "label": nullStr(label),
			"codeSystem": nullStr(system), "drugCode": nullStr(code), "status": nullStr(st),
			"prescribedAt": nullStr(at), "dosage": nullStr(dosage), "_raw": raw})
	}
	s.respondWithWarnings(w, items, raws, single)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------- GET /patients/{id}/observations ----------

func (s *Server) patientObservations(w http.ResponseWriter, r *http.Request) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return
	}
	includeHidden, err := boolParam(r, "includeHidden")
	if paramError(w, err) {
		return
	}
	// GET .../{recordId} returns one record, even if it would be hidden.
	single := r.PathValue("recordId") != ""
	includeHidden = includeHidden || single
	from, err := dateParam(r, "from")
	if paramError(w, err) {
		return
	}
	to, err := dateParam(r, "to")
	if paramError(w, err) {
		return
	}
	q := `SELECT id, raw_resource_id, code_system, lab_test_code, name, label, status, measured_at, value, value_text, unit,
			is_primary, replaced_by, is_impossible, is_implausible
		FROM observations WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?))`
	args := []any{id}
	if code := r.URL.Query().Get("code"); code != "" {
		code = labShortcut(code)
		q += ` AND lab_test_code = ?`
		args = append(args, code)
	}
	if from != "" {
		q += ` AND measured_at >= ?`
		args = append(args, from)
	}
	if to != "" {
		q += ` AND substr(measured_at, 1, 10) <= ?`
		args = append(args, to)
	}
	if single {
		q += ` AND id = ?`
		args = append(args, r.PathValue("recordId"))
	}
	if !includeHidden {
		q += ` AND is_primary = 1 AND replaced_by IS NULL AND is_impossible = 0 AND coalesce(status, '') NOT IN ('entered-in-error', 'cancelled')`
	}
	rows, err := s.db.Query(q+` ORDER BY measured_at DESC, id`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	var items []map[string]any
	var raws []int64
	for rows.Next() {
		var oid string
		var raw int64
		var system, code, name, label, st, at, valueText, unit, replacedBy sql.NullString
		var value sql.NullFloat64
		var primary, impossible, implausible bool
		if err := rows.Scan(&oid, &raw, &system, &code, &name, &label, &st, &at, &value, &valueText, &unit,
			&primary, &replacedBy, &impossible, &implausible); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		test := name.String
		if test == "" {
			test = label.String
		}
		item := map[string]any{"id": oid, "test": nullIfEmpty(test), "label": nullStr(label), "codeSystem": nullStr(system),
			"code": nullStr(code), "date": nullStr(at), "value": nullFloat(value), "valueText": nullStr(valueText),
			"unit": nullStr(unit), "status": nullStr(st), "_raw": raw}
		if includeHidden {
			hidden := []string{}
			if replacedBy.Valid {
				hidden = append(hidden, "replaced by "+replacedBy.String)
			} else if !primary {
				hidden = append(hidden, "not the primary record in its duplicate group")
			}
			if impossible {
				hidden = append(hidden, "impossible value")
			}
			if st.String == "entered-in-error" || st.String == "cancelled" {
				hidden = append(hidden, "status "+st.String)
			}
			item["hiddenBecause"] = hidden
		}
		if implausible {
			item["implausible"] = true
		}
		raws = append(raws, raw)
		items = append(items, item)
	}
	rows.Close()

	// Components such as systolic and diastolic blood pressure.
	for _, it := range items {
		comps, err := s.components(it["id"].(string))
		if err != nil {
			serverError(w, err)
			return
		}
		if len(comps) > 0 {
			it["components"] = comps
		}
	}
	s.respondWithWarnings(w, items, raws, single)
}

func (s *Server) components(observationID string) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT lab_test_code, name, value, value_text, unit FROM observation_components
		WHERE observation_id = ? ORDER BY rowid`, observationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var code, name, valueText, unit sql.NullString
		var value sql.NullFloat64
		if err := rows.Scan(&code, &name, &value, &valueText, &unit); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"test": nullStr(name), "code": nullStr(code), "value": nullFloat(value),
			"valueText": nullStr(valueText), "unit": nullStr(unit)})
	}
	return out, rows.Err()
}

// labShortcut lets callers write ?code=a1c instead of the LOINC code.
func labShortcut(code string) string {
	shortcuts := map[string]string{
		"a1c": "4548-4", "hba1c": "4548-4", "glucose": "2339-0", "cholesterol": "2093-3",
		"weight": "29463-7", "heart-rate": "8867-4", "oxygen": "59408-5", "blood-pressure": "85354-9",
		"tsh": "11579-0", "smoking": "72166-2",
	}
	if c, ok := shortcuts[strings.ToLower(code)]; ok {
		return c
	}
	return code
}

// ---------- GET /patients/{id}/procedures ----------

func (s *Server) patientProcedures(w http.ResponseWriter, r *http.Request) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return
	}
	includeHidden, err := boolParam(r, "includeHidden")
	if paramError(w, err) {
		return
	}
	// GET .../{recordId} returns one record, even if it would be hidden.
	single := r.PathValue("recordId") != ""
	includeHidden = includeHidden || single
	q := `SELECT id, raw_resource_id, code_system, procedure_code, name, label, status, performed_at, performer
		FROM procedures WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?))`
	args := []any{id}
	if single {
		q += ` AND id = ?`
		args = append(args, r.PathValue("recordId"))
	}
	if !includeHidden {
		q += ` AND coalesce(status, '') <> 'entered-in-error'`
	}
	rows, err := s.db.Query(q+` ORDER BY performed_at DESC, id`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	var items []map[string]any
	var raws []int64
	for rows.Next() {
		var pid string
		var raw int64
		var system, code, name, label, st, at, performer sql.NullString
		if err := rows.Scan(&pid, &raw, &system, &code, &name, &label, &st, &at, &performer); err != nil {
			serverError(w, err)
			return
		}
		display := name.String
		if display == "" {
			display = label.String
		}
		raws = append(raws, raw)
		items = append(items, map[string]any{"id": pid, "procedure": nullIfEmpty(display), "label": nullStr(label),
			"codeSystem": nullStr(system), "procedureCode": nullStr(code), "status": nullStr(st),
			"performedAt": nullStr(at), "performer": nullStr(performer), "_raw": raw})
	}
	s.respondWithWarnings(w, items, raws, single)
}

// ---------- GET /patients/{id}/notes ----------

func (s *Server) patientNotes(w http.ResponseWriter, r *http.Request) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return
	}
	includeHidden, err := boolParam(r, "includeHidden")
	if paramError(w, err) {
		return
	}
	// GET .../{recordId} returns one record, even if it would be hidden.
	single := r.PathValue("recordId") != ""
	includeHidden = includeHidden || single
	q := `SELECT id, raw_resource_id, status, note_type, date, author FROM document_references WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?))`
	args := []any{id}
	// ?q= searches the text of this patient's notes. One patient has a few hundred
	// notes at most, so a plain text match is fast enough and easy to get right.
	if text := strings.TrimSpace(r.URL.Query().Get("q")); text != "" {
		q += ` AND id IN (SELECT a.document_id FROM document_attachments a JOIN binaries b ON b.id = a.binary_id
			WHERE b.text LIKE ? ESCAPE '\')`
		args = append(args, "%"+likeEscape(text)+"%")
	}
	if single {
		q += ` AND id = ?`
		args = append(args, r.PathValue("recordId"))
	}
	if !includeHidden {
		q += ` AND coalesce(status, '') <> 'entered-in-error'`
	}
	rows, err := s.db.Query(q+` ORDER BY date DESC, id`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	var items []map[string]any
	var raws []int64
	for rows.Next() {
		var did string
		var raw int64
		var st, noteType, date, author sql.NullString
		if err := rows.Scan(&did, &raw, &st, &noteType, &date, &author); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		raws = append(raws, raw)
		items = append(items, map[string]any{"id": did, "type": nullStr(noteType), "date": nullStr(date),
			"author": nullStr(author), "status": nullStr(st), "_raw": raw})
	}
	rows.Close()

	for _, it := range items {
		atts, err := s.attachments(it["id"].(string))
		if err != nil {
			serverError(w, err)
			return
		}
		it["attachments"] = atts
	}
	s.respondWithWarnings(w, items, raws, single)
}

func (s *Server) attachments(documentID string) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT a.binary_id, b.id IS NOT NULL, b.content_type, b.size_bytes, b.text, b.data IS NOT NULL
		FROM document_attachments a LEFT JOIN binaries b ON b.id = a.binary_id
		WHERE a.document_id = ? ORDER BY a.binary_id`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var binaryID string
		var found, hasData bool
		var contentType, text sql.NullString
		var size sql.NullInt64
		if err := rows.Scan(&binaryID, &found, &contentType, &size, &text, &hasData); err != nil {
			return nil, err
		}
		att := map[string]any{"binaryId": binaryID, "contentType": nullStr(contentType), "text": nullStr(text)}
		if size.Valid {
			att["sizeBytes"] = size.Int64
		}
		if !text.Valid {
			switch {
			case !found:
				att["textUnavailableReason"] = "the Binary this note points to is not in the data"
			case !hasData:
				att["textUnavailableReason"] = "the content could not be decoded or was too large"
			default:
				att["textUnavailableReason"] = fmt.Sprintf("content is %s, not text", contentType.String)
			}
		}
		out = append(out, att)
	}
	return out, rows.Err()
}

// ---------- GET /patients/{id}/timeline ----------

// The timeline puts every visible record in one list, newest first.
func (s *Server) patientTimeline(w http.ResponseWriter, r *http.Request) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return
	}
	rows, err := s.db.Query(`
		SELECT measured_at, 'observation', id, coalesce(name, label, 'Observation'),
		       CASE WHEN value IS NOT NULL THEN printf('%g', value) || coalesce(' ' || unit, '') ELSE value_text END
		  FROM observations WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?)) AND is_primary = 1 AND replaced_by IS NULL AND is_impossible = 0
		       AND coalesce(status, '') NOT IN ('entered-in-error', 'cancelled')
		UNION ALL
		SELECT onset_date, 'condition', id, coalesce(name, label, 'Unspecified condition'), clinical_status
		  FROM conditions WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?)) AND coalesce(verification_status, '') <> 'entered-in-error'
		UNION ALL
		SELECT prescribed_at, 'medication', id, coalesce(name, label, 'Medication'), dosage
		  FROM medications WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?)) AND coalesce(status, '') <> 'entered-in-error'
		UNION ALL
		SELECT performed_at, 'procedure', id, coalesce(name, label, 'Procedure'), status
		  FROM procedures WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?)) AND coalesce(status, '') <> 'entered-in-error'
		UNION ALL
		SELECT date, 'note', id, coalesce(note_type, 'Note'), author
		  FROM document_references WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?)) AND coalesce(status, '') <> 'entered-in-error'`,
		id, id, id, id, id)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	events := []map[string]any{}
	for rows.Next() {
		var date, kind, rid, title, detail sql.NullString
		if err := rows.Scan(&date, &kind, &rid, &title, &detail); err != nil {
			serverError(w, err)
			return
		}
		events = append(events, map[string]any{"date": nullStr(date), "kind": kind.String, "id": rid.String,
			"title": title.String, "detail": nullStr(detail)})
	}
	// Newest first; records without a date go last.
	sort.SliceStable(events, func(i, j int) bool {
		di, _ := events[i]["date"].(string)
		dj, _ := events[j]["date"].(string)
		if di == "" || dj == "" {
			return di != ""
		}
		return di > dj
	})
	writeJSON(w, http.StatusOK, events)
}

// ---------- GET /patients/{id}/records ----------

// Raw records of any type for this patient, including types without a clean
// table yet (for example Immunization). Filter with ?type=.
func (s *Server) patientRawRecords(w http.ResponseWriter, r *http.Request) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return
	}
	q := `SELECT id, resource_type, raw_json FROM raw_resources WHERE patient_id IN (SELECT q.id FROM patients q WHERE q.person_id = (SELECT pp.person_id FROM patients pp WHERE pp.id = ?)) AND raw_json IS NOT NULL`
	args := []any{id}
	if t := r.URL.Query().Get("type"); t != "" {
		q += ` AND resource_type = ?`
		args = append(args, t)
	}
	rows, err := s.db.Query(q+` ORDER BY id`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var line int64
		var rt, raw string
		if err := rows.Scan(&line, &rt, &raw); err != nil {
			serverError(w, err)
			return
		}
		out = append(out, map[string]any{"line": line, "resourceType": rt, "format": "raw", "resource": json.RawMessage(redactRaw(raw))})
	}
	writeJSON(w, http.StatusOK, out)
}

// likeEscape makes % and _ in user text match literally in a LIKE pattern.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)
}
