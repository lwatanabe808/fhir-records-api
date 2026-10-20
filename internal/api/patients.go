package api

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// Endpoints for the patient app: one person looking at their own records.
//
// Records that shouldn't count as real data (entered in error, replaced by a
// correction, extra duplicates, impossible values) are never shown here. The
// data team sees them through /data-quality/issues.
//
// Each response is a small struct. A *string field is JSON null when the
// database value is NULL.

// visibleObservation matches the observations the patient app shows.
const visibleObservation = `is_primary = 1 AND replaced_by IS NULL AND is_impossible = 0
	AND coalesce(status, '') NOT IN ('entered-in-error', 'cancelled')`

// ---------- GET /patients ----------

type patientListItem struct {
	ID        string  `json:"id"`
	Name      *string `json:"name"`
	Gender    *string `json:"gender"`
	BirthDate *string `json:"birthDate"`
	Age       *int    `json:"age"`
}

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

	// Build the filter from the parameters that were given.
	where, args := "1=1", []any{}
	if name := r.URL.Query().Get("name"); name != "" {
		where += " AND lower(full_name) LIKE ?"
		args = append(args, "%"+strings.ToLower(name)+"%")
	}
	if gender != "" {
		where += " AND gender = ?"
		args = append(args, gender)
	}
	if bornAfter != "" {
		where += " AND birth_date >= ?"
		args = append(args, bornAfter)
	}
	if bornBefore != "" {
		where += " AND birth_date <= ?"
		args = append(args, bornBefore)
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM patients WHERE `+where, args...).Scan(&total); err != nil {
		serverError(w, err)
		return
	}
	rows, err := s.db.Query(`SELECT id, full_name, gender, birth_date FROM patients WHERE `+where+`
		ORDER BY family_name, given_names, id LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	patients := []patientListItem{}
	for rows.Next() {
		var p patientListItem
		if err := rows.Scan(&p.ID, &p.Name, &p.Gender, &p.BirthDate); err != nil {
			serverError(w, err)
			return
		}
		p.Age = s.age(p.BirthDate)
		patients = append(patients, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "limit": limit, "offset": offset, "patients": patients})
}

// ---------- GET /patients/{id} ----------

type patient struct {
	ID           string         `json:"id"`
	Name         *string        `json:"name"`
	FamilyName   *string        `json:"familyName"`
	GivenNames   *string        `json:"givenNames"`
	Gender       *string        `json:"gender"`
	BirthDate    *string        `json:"birthDate"`
	Age          *int           `json:"age"`
	Active       *bool          `json:"active"`
	RecordCounts map[string]int `json:"recordCounts"`
	Warnings     []warning      `json:"warnings"`
}

func (s *Server) getPatient(w http.ResponseWriter, r *http.Request) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return
	}
	p := patient{ID: id, RecordCounts: map[string]int{}}
	var raw int64
	err := s.db.QueryRow(`SELECT raw_resource_id, full_name, family_name, given_names, gender, birth_date, active
		FROM patients WHERE id = ?`, id).Scan(&raw, &p.Name, &p.FamilyName, &p.GivenNames, &p.Gender, &p.BirthDate, &p.Active)
	if err != nil {
		serverError(w, err)
		return
	}
	p.Age = s.age(p.BirthDate)

	// How many records of each kind this patient has.
	for key, table := range map[string]string{"conditions": "conditions", "medications": "medications",
		"observations": "observations", "procedures": "procedures", "notes": "document_references"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE patient_id = ?`, id).Scan(&n); err != nil {
			serverError(w, err)
			return
		}
		p.RecordCounts[key] = n
	}

	// list of warnings
	warnings, err := s.warningsFor([]int64{raw})
	if err != nil {
		serverError(w, err)
		return
	}
	p.Warnings = warnings.on(raw)
	writeJSON(w, http.StatusOK, p)
}

// ---------- GET /patients/{id}/conditions ----------

type condition struct {
	ID           string    `json:"id"`
	Condition    string    `json:"condition"` // the code's name, else the clinic's label
	Label        *string   `json:"label"`
	CodeSystem   *string   `json:"codeSystem"`
	DiseaseCode  *string   `json:"diseaseCode"`
	Status       *string   `json:"status"`
	Verification *string   `json:"verification"`
	Onset        *string   `json:"onset"`
	Warnings     []warning `json:"warnings"`
	raw          int64     // line in the file, for looking up warnings
}

func (s *Server) patientConditions(w http.ResponseWriter, r *http.Request) {
	list, ok := s.newRecordList(w, r, `SELECT id, raw_resource_id, coalesce(name, label, 'Unspecified condition'), label,
		code_system, disease_code, clinical_status, verification_status, onset_date FROM conditions`)
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
	list.where(`coalesce(verification_status, '') <> 'entered-in-error'`) // records that shouldn\'t count are never shown

	rows, err := s.db.Query(list.query+` ORDER BY onset_date DESC, id`, list.args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	conditions := []condition{}
	var raws []int64
	for rows.Next() {
		var c condition
		if err := rows.Scan(&c.ID, &c.raw, &c.Condition, &c.Label, &c.CodeSystem, &c.DiseaseCode,
			&c.Status, &c.Verification, &c.Onset); err != nil {
			serverError(w, err)
			return
		}
		conditions = append(conditions, c)
		raws = append(raws, c.raw)
	}

	warnings, err := s.warningsFor(raws)
	if err != nil {
		serverError(w, err)
		return
	}
	for i := range conditions {
		conditions[i].Warnings = warnings.on(conditions[i].raw)
	}
	writeJSON(w, http.StatusOK, conditions)
}

// ---------- GET /patients/{id}/medications ----------

type medication struct {
	ID           string    `json:"id"`
	Medication   *string   `json:"medication"` // the code's name, else the clinic's label
	Label        *string   `json:"label"`
	CodeSystem   *string   `json:"codeSystem"`
	DrugCode     *string   `json:"drugCode"`
	Status       *string   `json:"status"`
	PrescribedAt *string   `json:"prescribedAt"`
	Dosage       *string   `json:"dosage"`
	Warnings     []warning `json:"warnings"`
	raw          int64
}

func (s *Server) patientMedications(w http.ResponseWriter, r *http.Request) {
	list, ok := s.newRecordList(w, r, `SELECT id, raw_resource_id, coalesce(name, label), label,
		code_system, drug_code, status, prescribed_at, dosage FROM medications`)
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
	list.where(`coalesce(status, '') <> 'entered-in-error'`) // records that shouldn\'t count are never shown

	rows, err := s.db.Query(list.query+` ORDER BY prescribed_at DESC, id`, list.args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	medications := []medication{}
	var raws []int64
	for rows.Next() {
		var m medication
		if err := rows.Scan(&m.ID, &m.raw, &m.Medication, &m.Label, &m.CodeSystem, &m.DrugCode,
			&m.Status, &m.PrescribedAt, &m.Dosage); err != nil {
			serverError(w, err)
			return
		}
		medications = append(medications, m)
		raws = append(raws, m.raw)
	}

	warnings, err := s.warningsFor(raws)
	if err != nil {
		serverError(w, err)
		return
	}
	for i := range medications {
		medications[i].Warnings = warnings.on(medications[i].raw)
	}
	writeJSON(w, http.StatusOK, medications)
}

// ---------- GET /patients/{id}/observations ----------

type observation struct {
	ID         string      `json:"id"`
	Test       *string     `json:"test"` // the code's name, else the clinic's label
	Label      *string     `json:"label"`
	CodeSystem *string     `json:"codeSystem"`
	Code       *string     `json:"code"`
	Date       *string     `json:"date"`
	Status     *string     `json:"status"`
	Value      *float64    `json:"value"`
	ValueText  *string     `json:"valueText"`
	Unit       *string     `json:"unit"`
	Components []component `json:"components,omitempty"` // e.g. systolic and diastolic
	Warnings   []warning   `json:"warnings"`
	raw        int64
}

// component is one part of a multi-part result, like systolic blood pressure.
type component struct {
	Test      *string  `json:"test"`
	Code      *string  `json:"code"`
	Value     *float64 `json:"value"`
	ValueText *string  `json:"valueText"`
	Unit      *string  `json:"unit"`
}

func (s *Server) patientObservations(w http.ResponseWriter, r *http.Request) {
	// start to build the LIST query
	list, ok := s.newRecordList(w, r, `SELECT id, raw_resource_id, coalesce(name, label), label,
		code_system, lab_test_code, measured_at, status FROM observations`)
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
		list.where(`lab_test_code = ?`, code) // a LOINC code, e.g. 4548-4 for A1c
	}
	if from != "" {
		list.where(`measured_at >= ?`, from)
	}
	if to != "" {
		list.where(`substr(measured_at, 1, 10) <= ?`, to)
	}
	list.where(visibleObservation) // records that shouldn\'t count are never shown

	rows, err := s.db.Query(list.query+` ORDER BY measured_at DESC, id`, list.args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	observations := []observation{}
	var raws []int64
	for rows.Next() {
		var o observation
		if err := rows.Scan(&o.ID, &o.raw, &o.Test, &o.Label, &o.CodeSystem, &o.Code, &o.Date, &o.Status); err != nil {
			serverError(w, err)
			return
		}
		observations = append(observations, o)
		raws = append(raws, o.raw)
	}

	// Now that the query is done, add every observation's values and warnings.
	// populate the observation results
	if err := s.addValues(observations); err != nil {
		serverError(w, err)
		return
	}
	warnings, err := s.warningsFor(raws)
	if err != nil {
		serverError(w, err)
		return
	}
	for i := range observations {
		observations[i].Warnings = warnings.on(observations[i].raw)
	}
	writeJSON(w, http.StatusOK, observations)
}

// addValues fills in the results of every observation in the list with one
// query: the single value, or the components of a multi-part result like
// blood pressure.
func (s *Server) addValues(observations []observation) error {
	if len(observations) == 0 {
		return nil
	}
	byID := map[string]*observation{}
	ids := make([]any, len(observations))
	for i := range observations {
		byID[observations[i].ID] = &observations[i]
		ids[i] = observations[i].ID
	}
	rows, err := s.db.Query(`SELECT observation_id, is_component, name, lab_test_code, value, value_text, unit
		FROM observation_values WHERE observation_id IN (`+placeholders(len(ids))+`) ORDER BY rowid`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var isComponent bool
		var c component
		if err := rows.Scan(&id, &isComponent, &c.Test, &c.Code, &c.Value, &c.ValueText, &c.Unit); err != nil {
			return err
		}
		o := byID[id]
		if isComponent {
			o.Components = append(o.Components, c)
		} else {
			o.Value, o.ValueText, o.Unit = c.Value, c.ValueText, c.Unit
		}
	}
	return rows.Err()
}

// ---------- GET /patients/{id}/procedures ----------

type procedure struct {
	ID            string    `json:"id"`
	Procedure     *string   `json:"procedure"` // the code's name, else the clinic's label
	Label         *string   `json:"label"`
	CodeSystem    *string   `json:"codeSystem"`
	ProcedureCode *string   `json:"procedureCode"`
	Status        *string   `json:"status"`
	PerformedAt   *string   `json:"performedAt"`
	Performer     *string   `json:"performer"`
	Warnings      []warning `json:"warnings"`
	raw           int64
}

func (s *Server) patientProcedures(w http.ResponseWriter, r *http.Request) {
	// starts to build the list query
	list, ok := s.newRecordList(w, r, `SELECT id, raw_resource_id, coalesce(name, label), label,
		code_system, procedure_code, status, performed_at, performer FROM procedures`)
	if !ok {
		return
	}
	list.where(`coalesce(status, '') <> 'entered-in-error'`) // records that shouldn\'t count are never shown

	rows, err := s.db.Query(list.query+` ORDER BY performed_at DESC, id`, list.args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	procedures := []procedure{}
	var raws []int64
	for rows.Next() {
		var p procedure
		if err := rows.Scan(&p.ID, &p.raw, &p.Procedure, &p.Label, &p.CodeSystem, &p.ProcedureCode,
			&p.Status, &p.PerformedAt, &p.Performer); err != nil {
			serverError(w, err)
			return
		}
		procedures = append(procedures, p)
		raws = append(raws, p.raw)
	}

	warnings, err := s.warningsFor(raws)
	if err != nil {
		serverError(w, err)
		return
	}
	for i := range procedures {
		procedures[i].Warnings = warnings.on(procedures[i].raw)
	}
	writeJSON(w, http.StatusOK, procedures)
}

// ---------- GET /patients/{id}/notes ----------

type note struct {
	ID          string       `json:"id"`
	Type        *string      `json:"type"`
	Date        *string      `json:"date"`
	Author      *string      `json:"author"`
	Status      *string      `json:"status"`
	Attachments []attachment `json:"attachments"`
	Warnings    []warning    `json:"warnings"`
	raw         int64
}

// attachment is one file of a note, with its text when it's readable.
type attachment struct {
	BinaryID              string  `json:"binaryId"`
	ContentType           *string `json:"contentType"`
	SizeBytes             *int64  `json:"sizeBytes,omitempty"`
	Text                  *string `json:"text"`
	TextUnavailableReason string  `json:"textUnavailableReason,omitempty"`
}

func (s *Server) patientNotes(w http.ResponseWriter, r *http.Request) {
	list, ok := s.newRecordList(w, r, `SELECT id, raw_resource_id, note_type, date, author, status FROM document_references`)
	if !ok {
		return
	}
	list.where(`coalesce(status, '') <> 'entered-in-error'`) // records that shouldn\'t count are never shown

	rows, err := s.db.Query(list.query+` ORDER BY date DESC, id`, list.args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	notes := []note{}
	var raws []int64
	for rows.Next() {
		var n note
		if err := rows.Scan(&n.ID, &n.raw, &n.Type, &n.Date, &n.Author, &n.Status); err != nil {
			serverError(w, err)
			return
		}
		notes = append(notes, n)
		raws = append(raws, n.raw)
	}

	// Now that the query is done, add every note's attachments and warnings.
	if err := s.addAttachments(notes); err != nil {
		serverError(w, err)
		return
	}
	warnings, err := s.warningsFor(raws)
	if err != nil {
		serverError(w, err)
		return
	}
	for i := range notes {
		notes[i].Warnings = warnings.on(notes[i].raw)
	}
	writeJSON(w, http.StatusOK, notes)
}

// addAttachments fills in the files of every note in the list with one query.
// When a file has no readable text, it says why.
func (s *Server) addAttachments(notes []note) error {
	if len(notes) == 0 {
		return nil
	}
	byID := map[string]*note{}
	ids := make([]any, len(notes))
	for i := range notes {
		notes[i].Attachments = []attachment{}
		byID[notes[i].ID] = &notes[i]
		ids[i] = notes[i].ID
	}
	rows, err := s.db.Query(`SELECT a.document_id, a.binary_id, b.id IS NOT NULL, b.content_type, b.size_bytes, b.text, b.data IS NOT NULL
		FROM document_attachments a LEFT JOIN binaries b ON b.id = a.binary_id
		WHERE a.document_id IN (`+placeholders(len(ids))+`) ORDER BY a.binary_id`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var noteID string
		var a attachment
		var found, hasData bool
		if err := rows.Scan(&noteID, &a.BinaryID, &found, &a.ContentType, &a.SizeBytes, &a.Text, &hasData); err != nil {
			return err
		}
		switch {
		case a.Text != nil:
			// readable text: nothing to explain
		case !found:
			a.TextUnavailableReason = "the Binary this note points to is not in the data"
		case !hasData:
			a.TextUnavailableReason = "the content could not be decoded or was too large"
		default:
			a.TextUnavailableReason = fmt.Sprintf("content is %s, not text", *a.ContentType)
		}
		n := byID[noteID]
		n.Attachments = append(n.Attachments, a)
	}
	return rows.Err()
}

// ---------- GET /patients/{id}/timeline ----------

type event struct {
	Date   *string `json:"date"`
	Kind   string  `json:"kind"` // observation, condition, medication, procedure or note
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Detail *string `json:"detail"`
}

// patientTimeline puts every visible record in one list, newest first.
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
	events := []event{}
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.Date, &e.Kind, &e.ID, &e.Title, &e.Detail); err != nil {
			serverError(w, err)
			return
		}
		events = append(events, e)
	}

	// Newest first; records without a date go last.
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i].Date, events[j].Date
		if a == nil || b == nil {
			return a != nil
		}
		return *a > *b
	})
	writeJSON(w, http.StatusOK, events)
}

// ---------- Shared helpers ----------

// finds the patient for {id}.
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

// recordList is the query for one of a patient's record lists, built up
// filter by filter.
type recordList struct {
	query string
	args  []any
}

// newRecordList starts a record list query for the patient in the URL.
// ok is false when an error response was already written.
func (s *Server) newRecordList(w http.ResponseWriter, r *http.Request, selectFrom string) (list *recordList, ok bool) {
	id := s.resolvePatient(w, r)
	if id == "" {
		return nil, false
	}
	return &recordList{query: selectFrom + ` WHERE patient_id = ?`, args: []any{id}}, true
}

// where adds one filter to the query.
func (l *recordList) where(cond string, args ...any) {
	l.query += ` AND ` + cond
	l.args = append(l.args, args...)
}

// warning is one problem found in a record, from the issues table.
type warning struct {
	Type   string `json:"type"`
	Detail string `json:"detail,omitempty"`
}

// warningsByLine holds the warnings for each raw line.
type warningsByLine map[int64][]warning

// on returns the warnings for one line, or an empty list.
func (w warningsByLine) on(line int64) []warning {
	if w[line] == nil {
		return []warning{}
	}
	return w[line]
}

// warningsFor looks up the issues logged for the given raw lines, so a
// response can say what was wrong with each record.
func (s *Server) warningsFor(lines []int64) (warningsByLine, error) {
	out := warningsByLine{}
	if len(lines) == 0 {
		return out, nil
	}
	args := make([]any, len(lines))
	for i, line := range lines {
		args[i] = line
	}
	rows, err := s.db.Query(`SELECT raw_resource_id, issue_type, coalesce(detail, '') FROM issues
		WHERE raw_resource_id IN (`+placeholders(len(lines))+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var line int64
		var w warning
		if err := rows.Scan(&line, &w.Type, &w.Detail); err != nil {
			return nil, err
		}
		out[line] = append(out[line], w)
	}
	return out, rows.Err()
}

// placeholders returns "?, ?, ?" with n question marks, for a SQL IN (...) list.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
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

// age is the patient's age today, or nil without a full birth date.
func (s *Server) age(birthDate *string) *int {
	if birthDate == nil {
		return nil
	}
	if age, ok := ageOn(*birthDate, s.now()); ok {
		return &age
	}
	return nil
}

// ageOn is the age on a given day. ok is false unless birthDate is a full date.
func ageOn(birthDate string, now time.Time) (age int, ok bool) {
	b, err := time.Parse("2006-01-02", birthDate)
	if err != nil {
		return 0, false
	}
	age = now.Year() - b.Year()
	if now.YearDay() < b.YearDay() {
		age--
	}
	return age, true
}
