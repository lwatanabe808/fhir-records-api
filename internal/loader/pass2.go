package loader

import (
	"database/sql"
	"fmt"
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
		l.findDuplicatePatients,
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
	ids, err := l.patientIDs()
	if err != nil {
		return err
	}
	byLower := map[string][]string{}
	for id := range ids {
		byLower[strings.ToLower(id)] = append(byLower[strings.ToLower(id)], id)
	}
	resolve := func(ref string) (id string, caseMismatch bool, detail string) {
		_, target, _ := fhir.ParseRef(ref)
		if ids[target] {
			return target, false, ""
		}
		matches := byLower[strings.ToLower(target)]
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
	rows, err := l.tx.Query(`SELECT id, patient_ref FROM raw_resources WHERE patient_ref IS NOT NULL`)
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

func (l *Loader) patientIDs() (map[string]bool, error) {
	rows, err := l.tx.Query(`SELECT id FROM patients`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// checkLifeDates flags measurements dated before the patient was born or after they died.
func (l *Loader) checkLifeDates() error {
	// Dates are compared only to the precision of the patient's date, so a
	// birth date of "1971" compares against the year of the measurement.
	rows, err := l.tx.Query(`SELECT o.id, o.raw_resource_id, o.measured_at,
			CASE WHEN p.birth_date IS NOT NULL AND substr(o.measured_at, 1, length(p.birth_date)) < p.birth_date
			     THEN 'before the patient''s birth date ' || p.birth_date
			     ELSE 'after the patient''s date of death ' || p.deceased_date END
		FROM observations o JOIN patients p ON p.id = o.patient_id
		WHERE o.measured_at IS NOT NULL AND (
			(p.birth_date IS NOT NULL AND substr(o.measured_at, 1, length(p.birth_date)) < p.birth_date) OR
			(p.deceased_date IS NOT NULL AND substr(o.measured_at, 1, length(p.deceased_date)) > p.deceased_date))`)
	if err != nil {
		return err
	}
	var found []Issue
	for rows.Next() {
		var id, measured, reason string
		var raw int64
		if err := rows.Scan(&id, &raw, &measured, &reason); err != nil {
			rows.Close()
			return err
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
		if err := l.addIssue(i); err != nil {
			return err
		}
	}
	return nil
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

	for _, i := range found {
		if err := l.addIssue(i); err != nil {
			return err
		}
	}
	return nil
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
			coalesce(o.patient_id, '') || '|' || coalesce(o.code_system, '') || '|' || o.lab_test_code || '|' || o.measured_at AS grp,
			coalesce(o.value, '') || '|' || coalesce(o.value_text, '') || '|' || coalesce(o.unit, '') || '|' ||
			coalesce((SELECT group_concat(coalesce(c.lab_test_code, '') || '=' || coalesce(c.value, '') || coalesce(c.unit, ''), ',')
			          FROM observation_components c WHERE c.observation_id = o.id), '')
		FROM observations o
		WHERE o.patient_id IS NOT NULL AND o.lab_test_code IS NOT NULL AND o.measured_at LIKE '%T%'
		  AND coalesce(o.status, '') <> 'entered-in-error'
		ORDER BY grp, o.id`)
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
		if decided || len(amended) == 1 {
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
		var others []string
		for _, id := range ids {
			if id != m.id {
				others = append(others, id)
			}
		}
		if err := l.addIssue(Issue{RawResourceID: m.raw, RecordType: "Observation", RecordID: m.id, Type: DuplicateRecord, GroupKey: groupKey,
			Severity: severity, Detail: detail + strings.Join(others, ", "), ActionTaken: action}); err != nil {
			return err
		}
	}
	return nil
}

// newest picks the latest record by meta.lastUpdated, then issued.
// decided is false when the timestamps can't tell them apart.
func newest(rows []obsRow) (obsRow, bool) {
	if len(rows) == 1 {
		return rows[0], true
	}
	for _, field := range []func(obsRow) string{
		func(r obsRow) string { return r.lastUpdated },
		func(r obsRow) string { return r.issued },
	} {
		best, unique := rows[0], true
		for _, r := range rows[1:] {
			if field(r) == "" || field(best) == "" {
				unique = false
				break
			}
			if field(r) > field(best) {
				best = r
			} else if field(r) == field(best) {
				unique = false
			}
		}
		if unique {
			return best, true
		}
	}
	sorted := append([]obsRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].id < sorted[j].id })
	return sorted[0], false
}

// ---------- Duplicate patients ----------

// findDuplicatePatients flags patients with the same name and birth date.
// They are never merged: mixing two people's records is worse than a duplicate.
func (l *Loader) findDuplicatePatients() error {
	rows, err := l.tx.Query(`SELECT p.id, p.raw_resource_id, p.full_name, p.birth_date,
			(SELECT group_concat(q.id, ', ') FROM patients q
			 WHERE lower(q.full_name) = lower(p.full_name) AND q.birth_date = p.birth_date AND q.id <> p.id)
		FROM patients p
		WHERE p.full_name IS NOT NULL AND p.birth_date IS NOT NULL`)
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
				Severity: Warning, Detail: fmt.Sprintf("%s, born %s, also appears as %s", name, birth, others.String), ActionTaken: "kept both, never merged"})
		}
	}
	rows.Close()
	for _, i := range found {
		if err := l.addIssue(i); err != nil {
			return err
		}
	}
	return nil
}
