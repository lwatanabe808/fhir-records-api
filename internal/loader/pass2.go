package loader

import (
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/lwatanabe808/novellia-fhir-api/internal/fhir"
)

// secondPass runs after every line is loaded, so links can point anywhere in the file.
func (l *Loader) secondPass() error {
	steps := []func() error{
		l.linkPatients,
		l.checkLifeDates,
		l.checkAttachments,
		l.groupObservations,
		l.matchBySSN,
		l.flagSameNameAndBirth,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Patient links ----------

// Tables whose rows carry a patient link, with the FHIR type they hold.
var linkedTables = []struct{ table, resourceType string }{
	{"observations", "Observation"},
	{"conditions", "Condition"},
	{"medications", "MedicationRequest"},
	{"procedures", "Procedure"},
	{"document_references", "DocumentReference"},
}

// linkPatients resolves every "Patient/<id>" link.
//   - exact match: linked
//   - match ignoring capital letters, and exactly one such patient: linked and flagged
//     (the FHIR spec says ids are case-insensitive)
//   - anything else: left unlinked and flagged. We never guess.
func (l *Loader) linkPatients() error {
	// Patient ids grouped by their lowercase form: "p1" → ["P1", "p1"].
	byLower := map[string][]string{}
	rows, err := l.tx.Query(`SELECT id FROM patients`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		byLower[strings.ToLower(id)] = append(byLower[strings.ToLower(id)], id)
	}
	rows.Close()

	resolve := func(ref string) (id string, caseMismatch bool, detail string) {
		_, target, _ := fhir.ParseRef(ref)
		matches := byLower[strings.ToLower(target)]
		if slices.Contains(matches, target) {
			return target, false, ""
		}
		switch len(matches) {
		case 1:
			return matches[0], true, ""
		case 0:
			return "", false, fmt.Sprintf("%s points to a patient that is not in the file", ref)
		default:
			return "", false, fmt.Sprintf("%s matches %d patients when ignoring capital letters", ref, len(matches))
		}
	}

	for _, t := range linkedTables {
		rows, err := l.tx.Query(`SELECT id, raw_resource_id, patient_ref FROM ` + t.table + ` WHERE patient_ref IS NOT NULL`)
		if err != nil {
			return err
		}
		type pending struct {
			id, ref string
			raw     int64
		}
		var list []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.raw, &p.ref); err != nil {
				rows.Close()
				return err
			}
			list = append(list, p)
		}
		rows.Close()

		for _, p := range list {
			patientID, mismatch, detail := resolve(p.ref)
			base := Issue{RawResourceID: p.raw, RecordType: t.resourceType, RecordID: p.id, Field: "subject.reference", Severity: Warning}
			switch {
			case patientID == "":
				base.Type, base.Detail, base.ActionTaken = UnknownPatient, detail, "left unlinked"
				if err := l.addIssue(base); err != nil {
					return err
				}
				continue
			case mismatch:
				base.Type = CaseMismatchReference
				base.Detail = fmt.Sprintf("%s differs from patient %q only in capital letters", p.ref, patientID)
				base.ActionTaken = "linked to " + patientID
				if err := l.addIssue(base); err != nil {
					return err
				}
			}
			if _, err := l.tx.Exec(`UPDATE `+t.table+` SET patient_id = ? WHERE id = ?`, patientID, p.id); err != nil {
				return err
			}
		}
	}

	// Raw records (including unsupported types) get the same links, without issues,
	// so /patients/{id}/records can find them.
	rows, err = l.tx.Query(`SELECT id, patient_ref FROM raw_resources WHERE patient_ref IS NOT NULL`)
	if err != nil {
		return err
	}
	links := map[int64]string{}
	for rows.Next() {
		var id int64
		var ref string
		if err := rows.Scan(&id, &ref); err != nil {
			rows.Close()
			return err
		}
		if patientID, _, _ := resolve(ref); patientID != "" {
			links[id] = patientID
		}
	}
	rows.Close()
	for id, patientID := range links {
		if _, err := l.tx.Exec(`UPDATE raw_resources SET patient_id = ? WHERE id = ?`, patientID, id); err != nil {
			return err
		}
	}
	return nil
}

// checkLifeDates flags measurements dated before the patient was born or after they died.
func (l *Loader) checkLifeDates() error {
	// Dates are compared only to the precision of the patient's date, so a
	// birth date of "1971" compares against the year of the measurement.
	rows, err := l.tx.Query(`SELECT o.id, o.raw_resource_id, o.measured_at,
			substr(o.measured_at, 1, length(p.birth_date)) < p.birth_date AS before_birth, p.birth_date, p.deceased_date
		FROM observations o JOIN patients p ON p.id = o.patient_id
		WHERE o.measured_at IS NOT NULL AND (
			(p.birth_date IS NOT NULL AND substr(o.measured_at, 1, length(p.birth_date)) < p.birth_date) OR
			(p.deceased_date IS NOT NULL AND substr(o.measured_at, 1, length(p.deceased_date)) > p.deceased_date))`)
	if err != nil {
		return err
	}
	var found []Issue
	for rows.Next() {
		var id, measured string
		var raw int64
		var beforeBirth sql.NullBool
		var birth, death sql.NullString
		if err := rows.Scan(&id, &raw, &measured, &beforeBirth, &birth, &death); err != nil {
			rows.Close()
			return err
		}
		reason := "after the patient's date of death " + death.String
		if beforeBirth.Bool {
			reason = "before the patient's birth date " + birth.String
		}
		found = append(found, Issue{RawResourceID: raw, RecordType: "Observation", RecordID: id, Type: ImpossibleValue, Severity: Error,
			Field: "effectiveDateTime", Detail: fmt.Sprintf("measured %s, %s", measured, reason),
			ActionTaken: "hidden from default views and research"})
	}
	rows.Close()
	for _, i := range found {
		if _, err := l.tx.Exec(`UPDATE observations SET is_impossible = 1 WHERE id = ?`, i.RecordID); err != nil {
			return err
		}
	}
	return l.addIssues(found)
}

// ---------- Attachments ----------

func (l *Loader) checkAttachments() error {
	var found []Issue

	// Notes that link to a Binary that isn't in the file.
	rows, err := l.tx.Query(`SELECT a.document_id, d.raw_resource_id, a.binary_id
		FROM document_attachments a JOIN document_references d ON d.id = a.document_id
		LEFT JOIN binaries b ON b.id = a.binary_id WHERE b.id IS NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var doc, bin string
		var raw int64
		if err := rows.Scan(&doc, &raw, &bin); err != nil {
			rows.Close()
			return err
		}
		found = append(found, Issue{RawResourceID: raw, RecordType: "DocumentReference", RecordID: doc, Type: BinaryBrokenReference,
			Severity: Warning, Field: "content.attachment.url", Detail: "Binary/" + bin + " is not in the file", ActionTaken: "note has no text for this attachment"})
	}
	rows.Close()

	// Note and Binary disagree on the content type: trust the Binary.
	rows, err = l.tx.Query(`SELECT a.document_id, d.raw_resource_id, a.binary_id, a.content_type, b.content_type
		FROM document_attachments a JOIN document_references d ON d.id = a.document_id
		JOIN binaries b ON b.id = a.binary_id
		WHERE b.is_inline = 0 AND a.content_type IS NOT NULL AND a.content_type <> b.content_type`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var doc, bin, noteType, binType string
		var raw int64
		if err := rows.Scan(&doc, &raw, &bin, &noteType, &binType); err != nil {
			rows.Close()
			return err
		}
		found = append(found, Issue{RawResourceID: raw, RecordType: "DocumentReference", RecordID: doc, Type: ContentTypeMismatch,
			Severity: Warning, Field: "content.attachment.contentType",
			Detail: fmt.Sprintf("note says %s, Binary/%s says %s", noteType, bin, binType), ActionTaken: "trusted the Binary's type"})
	}
	rows.Close()

	// Binaries that no note links to. They have no patient of their own.
	rows, err = l.tx.Query(`SELECT b.id, b.raw_resource_id FROM binaries b
		LEFT JOIN document_attachments a ON a.binary_id = b.id
		WHERE a.binary_id IS NULL AND b.is_inline = 0`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var bin string
		var raw int64
		if err := rows.Scan(&bin, &raw); err != nil {
			rows.Close()
			return err
		}
		found = append(found, Issue{RawResourceID: raw, RecordType: "Binary", RecordID: bin, Type: OrphanBinary, Severity: Info,
			Detail: "no DocumentReference links to this Binary, so its patient is unknown", ActionTaken: "kept, not shown on any chart"})
	}
	rows.Close()
	return l.addIssues(found)
}

// ---------- Duplicate and corrected observations ----------

type obsRow struct {
	id, status, lastUpdated, issued string
	raw                             int64
	signature                       string // value, text, unit and components, for comparing
}

// groupObservations finds observations with different ids but the same
// patient, lab test code and exact time (date-only times are never grouped).
//   - An amended or corrected record replaces the originals (SUPERSEDED).
//   - Otherwise the lowest id is primary and the others are hidden by default (DUPLICATE_RECORD).
func (l *Loader) groupObservations() error {
	rows, err := l.tx.Query(`SELECT o.id, coalesce(o.status, ''), coalesce(o.last_updated, ''), coalesce(o.issued, ''), o.raw_resource_id,
			o.patient_id || '|' || coalesce(o.code_system, '') || '|' || o.lab_test_code || '|' || o.measured_at AS grp,
			coalesce((SELECT group_concat(v.is_component || ':' || coalesce(v.lab_test_code, '') || '=' || coalesce(v.value, '') || '|' ||
			                              coalesce(v.value_text, '') || '|' || coalesce(v.unit, ''), ',')
			          FROM (SELECT * FROM observation_values WHERE observation_id = o.id ORDER BY rowid) v), '')
		FROM observations o
		WHERE o.patient_id IS NOT NULL AND o.lab_test_code IS NOT NULL AND o.measured_at LIKE '%T%'
		  AND coalesce(o.status, '') <> 'entered-in-error'
		ORDER BY o.patient_id, o.lab_test_code, o.measured_at, o.code_system, o.id`)
	if err != nil {
		return err
	}
	groups := map[string][]obsRow{}
	var order []string
	for rows.Next() {
		var r obsRow
		var grp string
		if err := rows.Scan(&r.id, &r.status, &r.lastUpdated, &r.issued, &r.raw, &grp, &r.signature); err != nil {
			rows.Close()
			return err
		}
		if _, ok := groups[grp]; !ok {
			order = append(order, grp)
		}
		groups[grp] = append(groups[grp], r)
	}
	rows.Close()

	for _, key := range order {
		members := groups[key]
		if len(members) < 2 {
			continue
		}
		if err := l.resolveGroup(key, members); err != nil {
			return err
		}
	}
	return nil
}

func (l *Loader) resolveGroup(groupKey string, members []obsRow) error {
	var amended, originals []obsRow
	for _, m := range members {
		if m.status == "amended" || m.status == "corrected" {
			amended = append(amended, m)
		} else {
			originals = append(originals, m)
		}
	}

	if len(amended) > 0 {
		winner, decided := newest(amended)
		replaced := originals
		if decided {
			for _, a := range amended {
				if a.id != winner.id {
					replaced = append(replaced, a)
				}
			}
		}
		for _, r := range replaced {
			if _, err := l.tx.Exec(`UPDATE observations SET replaced_by = ?, is_primary = 0 WHERE id = ?`, winner.id, r.id); err != nil {
				return err
			}
			if err := l.addIssue(Issue{RawResourceID: r.raw, RecordType: "Observation", RecordID: r.id, Type: Superseded, Severity: Info, GroupKey: groupKey,
				Field: "status", Detail: fmt.Sprintf("replaced by %s (status %s)", winner.id, winner.status), ActionTaken: "hidden by default, kept as history"}); err != nil {
				return err
			}
		}
		if decided {
			return nil
		}
		// Several corrections and no way to tell which is newest: treat them as duplicates.
		members = amended
	}

	// Duplicates: lowest id is primary.
	sort.Slice(members, func(i, j int) bool { return members[i].id < members[j].id })
	primary := members[0]
	same := true
	var ids []string
	for _, m := range members {
		ids = append(ids, m.id)
		if m.signature != primary.signature {
			same = false
		}
	}
	severity, detail := Info, "same patient, lab test code, time and values as "
	if !same {
		severity, detail = Warning, "same patient, lab test code and time, but different values from "
	}
	for _, m := range members {
		action := "primary (lowest id)"
		if m.id != primary.id {
			action = "hidden by default, primary is " + primary.id
			if _, err := l.tx.Exec(`UPDATE observations SET is_primary = 0 WHERE id = ?`, m.id); err != nil {
				return err
			}
		}
		if err := l.addIssue(Issue{RawResourceID: m.raw, RecordType: "Observation", RecordID: m.id, Type: DuplicateRecord, GroupKey: groupKey,
			Severity: severity, Detail: detail + others(ids, m.id), ActionTaken: action}); err != nil {
			return err
		}
	}
	return nil
}

// newest picks the latest record by meta.lastUpdated, or by issued when that
// can't decide. decided is false when neither can, and then the lowest id is returned.
func newest(rows []obsRow) (winner obsRow, decided bool) {
	if len(rows) == 1 {
		return rows[0], true
	}
	if r, ok := latestBy(rows, func(r obsRow) string { return r.lastUpdated }); ok {
		return r, true
	}
	if r, ok := latestBy(rows, func(r obsRow) string { return r.issued }); ok {
		return r, true
	}
	lowest := rows[0]
	for _, r := range rows {
		if r.id < lowest.id {
			lowest = r
		}
	}
	return lowest, false
}

// latestBy returns the row with the latest value of field. ok is false when a
// row has no value, or when two rows share the latest value.
func latestBy(rows []obsRow, field func(obsRow) string) (latest obsRow, ok bool) {
	tied := false
	for i, r := range rows {
		switch {
		case field(r) == "":
			return obsRow{}, false
		case i == 0 || field(r) > field(latest):
			latest, tied = r, false
		case field(r) == field(latest):
			tied = true
		}
	}
	return latest, !tied
}

// ---------- Matching people ----------

type patientRow struct {
	id, nameKey, birth string
	raw                int64
}

// Patient records that belong to the same person:
//
//   - Same valid SSN, and the same name and birth date: the same person.
//     They share a person_id, so the patient chart combines them. The rows
//     are never merged, so a wrong match can be undone.
//   - Same SSN, but the name or birth date differ: a conflict. Kept apart on
//     the chart (a wrong guess would show a stranger's records), but counted
//     once in research, where overcounting is the bigger risk.
//   - Same name and birth date only: a possible duplicate. Kept apart on the
//     chart, counted once in research.

// matchBySSN handles the first two cases.
func (l *Loader) matchBySSN() error {
	rows, err := l.tx.Query(`SELECT ssn_hash, id, raw_resource_id, coalesce(name_key, ''), coalesce(birth_date, '')
		FROM patients WHERE ssn_hash IN (SELECT ssn_hash FROM patients WHERE ssn_hash IS NOT NULL GROUP BY ssn_hash HAVING COUNT(*) > 1)
		ORDER BY ssn_hash, id`)
	if err != nil {
		return err
	}
	groups := map[string][]patientRow{}
	var order []string
	for rows.Next() {
		var hash string
		var p patientRow
		if err := rows.Scan(&hash, &p.id, &p.raw, &p.nameKey, &p.birth); err != nil {
			rows.Close()
			return err
		}
		if _, ok := groups[hash]; !ok {
			order = append(order, hash)
		}
		groups[hash] = append(groups[hash], p)
	}
	rows.Close()

	for _, hash := range order {
		members := groups[hash]
		person := members[0].id // lowest id, since rows are sorted
		agree := true
		var ids []string
		for _, m := range members {
			ids = append(ids, m.id)
			if m.nameKey == "" || m.birth == "" || m.nameKey != members[0].nameKey || m.birth != members[0].birth {
				agree = false
			}
		}
		for _, m := range members {
			issue := Issue{RawResourceID: m.raw, RecordType: "Patient", RecordID: m.id, Field: "identifier", GroupKey: "person:" + person}
			if agree {
				if _, err := l.tx.Exec(`UPDATE patients SET person_id = ? WHERE id = ?`, person, m.id); err != nil {
					return err
				}
				issue.Type, issue.Severity = SamePersonBySSN, Info
				issue.Detail = "same SSN, name and birth date as " + others(ids, m.id)
				issue.ActionTaken = "combined on the patient chart as person " + person + ", counted once in research"
			} else {
				issue.Type, issue.Severity = SSNConflict, Warning
				issue.Detail = "shares an SSN with " + others(ids, m.id) + ", but the name or birth date differ"
				issue.ActionTaken = "kept separate on the chart, counted once in research"
			}
			if err := l.addIssue(issue); err != nil {
				return err
			}
		}
	}
	return nil
}

// flagSameNameAndBirth handles the third case: it flags patients with the same normalized name and birth
// date who aren't already confirmed as the same person.
func (l *Loader) flagSameNameAndBirth() error {
	rows, err := l.tx.Query(`SELECT p.id, p.raw_resource_id, p.full_name, p.birth_date,
			(SELECT group_concat(q.id, ', ') FROM patients q
			 WHERE q.name_key = p.name_key AND q.birth_date = p.birth_date AND q.person_id <> p.person_id)
		FROM patients p
		WHERE p.name_key IS NOT NULL AND p.birth_date IS NOT NULL ORDER BY p.id`)
	if err != nil {
		return err
	}
	var found []Issue
	for rows.Next() {
		var id, name, birth string
		var raw int64
		var others sql.NullString
		if err := rows.Scan(&id, &raw, &name, &birth, &others); err != nil {
			rows.Close()
			return err
		}
		if others.Valid {
			found = append(found, Issue{RawResourceID: raw, RecordType: "Patient", RecordID: id, Type: PossibleDuplicatePatient,
				Severity: Warning, Detail: fmt.Sprintf("%s, born %s, also appears as %s", name, birth, others.String),
				ActionTaken: "kept separate on the chart, counted once in research"})
		}
	}
	rows.Close()
	return l.addIssues(found)
}

// others lists every id except self, for issue details like "also appears as a, b".
func others(ids []string, self string) string {
	var out []string
	for _, id := range ids {
		if id != self {
			out = append(out, id)
		}
	}
	return strings.Join(out, ", ")
}
