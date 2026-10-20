package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// Endpoints for the patient app: one person looking at their own records.
// Default views hide records that shouldn't count as real data (entered in
// error, replaced by a correction, duplicates, impossible values).
// ?includeHidden=true shows everything, with the reason it would be hidden.

// visibleObservation matches what the default observation views show.
const visibleObservation = `is_primary = 1 AND replaced_by IS NULL AND is_impossible = 0
	AND coalesce(status, '') NOT IN ('entered-in-error', 'cancelled')`

// resolvePatient finds the patient for {id}. Ids work in any capitalization,
// as long as only one patient matches. It writes a 404 and returns "" otherwise.
func (s *Server) resolvePatient(w http.ResponseWriter, r *http.Request) string {
	id := r.PathValue("id")
	matches, err := s.stringList(`SELECT id FROM patients WHERE lower(id) = lower(?)`, id)
	if err != nil {
		serverError(w, err)
		return ""
	}
	if slices.Contains(matches, id) {
		return id
	}
	if len(matches) == 1 {
		return matches[0]
	}
	writeError(w, http.StatusNotFound, "PATIENT_NOT_FOUND", "no patient with id "+id)
	return ""
}

// stringList runs a query that returns one text column and collects the values.
func (s *Server) stringList(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// recordList is the query for one of a patient's record lists (conditions,
// medications, observations, procedures, notes), built up filter by filter.
type recordList struct {
	query         string
	args          []any
	includeHidden bool // ?includeHidden=true
}

// newRecordList reads what every record list needs: the patient and
// ?includeHidden. ok is false when an error response was already written.
func (s *Server) newRecordList(w http.ResponseWriter, r *http.Request, selectFrom string) (list *recordList, ok bool) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return nil, false
	}
	includeHidden, err := boolParam(r, "includeHidden")
	if paramError(w, err) {
		return nil, false
	}
	return &recordList{query: selectFrom + ` WHERE patient_id = ?`, args: []any{id}, includeHidden: includeHidden}, true
}

// where adds one filter to the query.
func (l *recordList) where(cond string, args ...any) {
	l.query += ` AND ` + cond
	l.args = append(l.args, args...)
}

// nameOr returns the coding's display name, else the record's own label, else fallback.
func nameOr(name, label sql.NullString, fallback string) string {
	switch {
	case name.String != "":
		return name.String
	case label.String != "":
		return label.String
	}
	return fallback
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
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE patient_id = ?`, id).Scan(&n); err != nil {
			serverError(w, err)
			return
		}
		counts[key] = n
	}
	var activeV any
	if active.Valid {
		activeV = active.Bool
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": nullStr(name), "familyName": nullStr(family), "givenNames": nullStr(given),
		"gender": nullStr(gender), "birthDate": nullStr(birth), "age": ageOn(birth.String, s.now()),
		"active": activeV, "recordCounts": counts, "warnings": emptyIfNil(warns[raw]),
	})
}

// ---------- GET /patients/{id}/conditions ----------

func (s *Server) patientConditions(w http.ResponseWriter, r *http.Request) {
	list, ok := s.newRecordList(w, r, `SELECT id, raw_resource_id, code_system, disease_code, name, label,
		clinical_status, verification_status, onset_date FROM conditions`)
	if !ok {
		return
	}
	status, err := enumParam(r, "status", "active", "recurrence", "relapse", "inactive", "remission", "resolved")
	if paramError(w, err) {
		return
	}
	if status != "" {
		list.where(`clinical_status = ?`, status)
	}
	if !list.includeHidden {
		list.where(`coalesce(verification_status, '') <> 'entered-in-error'`)
	}
	rows, err := s.db.Query(list.query+` ORDER BY onset_date DESC, id`, list.args...)
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
		raws = append(raws, raw)
		items = append(items, map[string]any{"id": cid, "condition": nameOr(name, label, "Unspecified condition"), "label": nullStr(label),
			"codeSystem": nullStr(system), "diseaseCode": nullStr(code), "status": nullStr(clinical),
			"verification": nullStr(verification), "onset": nullStr(onset)})
	}
	s.respondWithWarnings(w, items, raws)
}

// respondWithWarnings attaches the issue list to each item and writes the list.
// raws[i] is the raw line of items[i].
func (s *Server) respondWithWarnings(w http.ResponseWriter, items []map[string]any, raws []int64) {
	warns, err := s.warningsFor(raws)
	if err != nil {
		serverError(w, err)
		return
	}
	for i, it := range items {
		it["warnings"] = emptyIfNil(warns[raws[i]])
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, items)
}

// ---------- GET /patients/{id}/medications ----------

func (s *Server) patientMedications(w http.ResponseWriter, r *http.Request) {
	list, ok := s.newRecordList(w, r, `SELECT id, raw_resource_id, code_system, drug_code, name, label,
		status, prescribed_at, dosage FROM medications`)
	if !ok {
		return
	}
	status, err := enumParam(r, "status", "active", "on-hold", "cancelled", "completed", "stopped", "draft", "unknown")
	if paramError(w, err) {
		return
	}
	if status != "" {
		list.where(`status = ?`, status)
	}
	if !list.includeHidden {
		list.where(`coalesce(status, '') <> 'entered-in-error'`)
	}
	rows, err := s.db.Query(list.query+` ORDER BY prescribed_at DESC, id`, list.args...)
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
		raws = append(raws, raw)
		items = append(items, map[string]any{"id": mid, "medication": nullIfEmpty(nameOr(name, label, "")), "label": nullStr(label),
			"codeSystem": nullStr(system), "drugCode": nullStr(code), "status": nullStr(st),
			"prescribedAt": nullStr(at), "dosage": nullStr(dosage)})
	}
	s.respondWithWarnings(w, items, raws)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------- GET /patients/{id}/observations ----------

func (s *Server) patientObservations(w http.ResponseWriter, r *http.Request) {
	list, ok := s.newRecordList(w, r, `SELECT id, raw_resource_id, code_system, lab_test_code, name, label, status,
		measured_at, is_primary, replaced_by, is_impossible FROM observations`)
	if !ok {
		return
	}
	from, err := dateParam(r, "from")
	if paramError(w, err) {
		return
	}
	to, err := dateParam(r, "to")
	if paramError(w, err) {
		return
	}
	if code := r.URL.Query().Get("code"); code != "" {
		list.where(`lab_test_code = ?`, labShortcut(code))
	}
	if from != "" {
		list.where(`measured_at >= ?`, from)
	}
	if to != "" {
		list.where(`substr(measured_at, 1, 10) <= ?`, to)
	}
	if !list.includeHidden {
		list.where(visibleObservation)
	}
	rows, err := s.db.Query(list.query+` ORDER BY measured_at DESC, id`, list.args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	var items []map[string]any
	var raws []int64
	for rows.Next() {
		var oid string
		var raw int64
		var system, code, name, label, st, at, replacedBy sql.NullString
		var primary, impossible bool
		if err := rows.Scan(&oid, &raw, &system, &code, &name, &label, &st, &at,
			&primary, &replacedBy, &impossible); err != nil {
			serverError(w, err)
			return
		}
		item := map[string]any{"id": oid, "test": nullIfEmpty(nameOr(name, label, "")), "label": nullStr(label), "codeSystem": nullStr(system),
			"code": nullStr(code), "date": nullStr(at), "status": nullStr(st)}
		if list.includeHidden {
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
		raws = append(raws, raw)
		items = append(items, item)
	}

	for _, it := range items {
		if err := s.addValues(it); err != nil {
			serverError(w, err)
			return
		}
	}
	s.respondWithWarnings(w, items, raws)
}

// addValues adds an observation's results to its JSON from observation_values:
// value, valueText and unit for a single result, and "components" for a
// multi-part result like blood pressure.
func (s *Server) addValues(item map[string]any) error {
	rows, err := s.db.Query(`SELECT is_component, lab_test_code, name, value, value_text, unit FROM observation_values
		WHERE observation_id = ? ORDER BY rowid`, item["id"])
	if err != nil {
		return err
	}
	defer rows.Close()
	item["value"], item["valueText"], item["unit"] = nil, nil, nil
	var components []map[string]any
	for rows.Next() {
		var isComponent bool
		var code, name, valueText, unit sql.NullString
		var value sql.NullFloat64
		if err := rows.Scan(&isComponent, &code, &name, &value, &valueText, &unit); err != nil {
			return err
		}
		if isComponent {
			components = append(components, map[string]any{"test": nullStr(name), "code": nullStr(code), "value": nullFloat(value),
				"valueText": nullStr(valueText), "unit": nullStr(unit)})
		} else {
			item["value"], item["valueText"], item["unit"] = nullFloat(value), nullStr(valueText), nullStr(unit)
		}
	}
	if len(components) > 0 {
		item["components"] = components
	}
	return rows.Err()
}

// labShortcuts lets callers write ?code=a1c instead of the LOINC code.
var labShortcuts = map[string]string{
	"a1c": "4548-4", "hba1c": "4548-4", "glucose": "2339-0", "cholesterol": "2093-3",
	"weight": "29463-7", "heart-rate": "8867-4", "oxygen": "59408-5", "blood-pressure": "85354-9",
	"tsh": "11579-0", "smoking": "72166-2",
}

func labShortcut(code string) string {
	if c, ok := labShortcuts[strings.ToLower(code)]; ok {
		return c
	}
	return code
}

// ---------- GET /patients/{id}/procedures ----------

func (s *Server) patientProcedures(w http.ResponseWriter, r *http.Request) {
	list, ok := s.newRecordList(w, r, `SELECT id, raw_resource_id, code_system, procedure_code, name, label,
		status, performed_at, performer FROM procedures`)
	if !ok {
		return
	}
	if !list.includeHidden {
		list.where(`coalesce(status, '') <> 'entered-in-error'`)
	}
	rows, err := s.db.Query(list.query+` ORDER BY performed_at DESC, id`, list.args...)
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
		raws = append(raws, raw)
		items = append(items, map[string]any{"id": pid, "procedure": nullIfEmpty(nameOr(name, label, "")), "label": nullStr(label),
			"codeSystem": nullStr(system), "procedureCode": nullStr(code), "status": nullStr(st),
			"performedAt": nullStr(at), "performer": nullStr(performer)})
	}
	s.respondWithWarnings(w, items, raws)
}

// ---------- GET /patients/{id}/notes ----------

func (s *Server) patientNotes(w http.ResponseWriter, r *http.Request) {
	list, ok := s.newRecordList(w, r, `SELECT id, raw_resource_id, status, note_type, date, author FROM document_references`)
	if !ok {
		return
	}
	if !list.includeHidden {
		list.where(`coalesce(status, '') <> 'entered-in-error'`)
	}
	rows, err := s.db.Query(list.query+` ORDER BY date DESC, id`, list.args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	var items []map[string]any
	var raws []int64
	for rows.Next() {
		var did string
		var raw int64
		var st, noteType, date, author sql.NullString
		if err := rows.Scan(&did, &raw, &st, &noteType, &date, &author); err != nil {
			serverError(w, err)
			return
		}
		raws = append(raws, raw)
		items = append(items, map[string]any{"id": did, "type": nullStr(noteType), "date": nullStr(date),
			"author": nullStr(author), "status": nullStr(st)})
	}

	for _, it := range items {
		atts, err := s.attachments(it["id"].(string))
		if err != nil {
			serverError(w, err)
			return
		}
		it["attachments"] = atts
	}
	s.respondWithWarnings(w, items, raws)
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
		SELECT o.measured_at, 'observation', o.id, coalesce(o.name, o.label, 'Observation'),
		       CASE WHEN v.value IS NOT NULL THEN printf('%g', v.value) || coalesce(' ' || v.unit, '') ELSE v.value_text END
		  FROM observations o LEFT JOIN observation_values v ON v.observation_id = o.id AND v.is_component = 0
		  WHERE patient_id = ? AND `+visibleObservation+`
		UNION ALL
		SELECT onset_date, 'condition', id, coalesce(name, label, 'Unspecified condition'), clinical_status
		  FROM conditions WHERE patient_id = ? AND coalesce(verification_status, '') <> 'entered-in-error'
		UNION ALL
		SELECT prescribed_at, 'medication', id, coalesce(name, label, 'Medication'), dosage
		  FROM medications WHERE patient_id = ? AND coalesce(status, '') <> 'entered-in-error'
		UNION ALL
		SELECT performed_at, 'procedure', id, coalesce(name, label, 'Procedure'), status
		  FROM procedures WHERE patient_id = ? AND coalesce(status, '') <> 'entered-in-error'
		UNION ALL
		SELECT date, 'note', id, coalesce(note_type, 'Note'), author
		  FROM document_references WHERE patient_id = ? AND coalesce(status, '') <> 'entered-in-error'`,
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
