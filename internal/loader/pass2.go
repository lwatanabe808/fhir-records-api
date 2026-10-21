package loader

import (
	"fmt"
	"strings"

	"github.com/lwatanabe808/novellia-fhir-api/internal/fhir"
)

// secondPass runs after every line is loaded, so links can point anywhere in the file.
func (l *Loader) secondPass() error {
	steps := []func() error{
		l.linkPatients,
		l.checkAttachments,
		l.groupObservations,
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

// tablesWithPatients are the tables whose records belong to a patient, with their record types.
var tablesWithPatients = []struct{ table, resourceType string }{
	{"observations", "Observation"},
	{"conditions", "Condition"},
	{"medications", "MedicationRequest"},
	{"procedures", "Procedure"},
	{"document_references", "DocumentReference"},
}

// linkPatients resolves every "Patient/<id>" link. IDs are compared ignoring
// capital letters, because the FHIR spec says ids are case-insensitive.
//   - exactly one patient fits: linked (and flagged if the capitals differ)
//   - none, or several (like "abc" and "ABC", the same id used twice): left
//     unlinked and flagged. We never guess.
func (l *Loader) linkPatients() error {
	// list patients from the first pass
	patients, err := l.loadPatientIndex()
	if err != nil {
		return err
	}

	// record types
	for _, t := range tablesWithPatients {
		if err := l.linkTable(t.table, t.resourceType, patients); err != nil {
			return err
		}
	}
	return nil
}

// patientIndex holds every patient id, grouped by its lowercase form: "p1" → ["P1", "p1"].
type patientIndex map[string][]string

func (l *Loader) loadPatientIndex() (patientIndex, error) {
	rows, err := l.tx.Query(`
SELECT id
FROM patients`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	patients := patientIndex{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		key := strings.ToLower(id)
		patients[key] = append(patients[key], id)
	}
	return patients, rows.Err()
}

// find looks up a link like "Patient/p1", ignoring capital letters. It returns
// the patient id, whether the link's capitals differ from it, and, when id is
// "", why it couldn't link.
func (patients patientIndex) find(ref string) (id string, caseMismatch bool, problem string) {
	// Patient/noah-wyle => noah-wyle
	// essentially parses by forward slash
	_, target, _ := fhir.ParseRef(ref)
	// because of toLower, we could have case mismatches that we need to log
	matches := patients[strings.ToLower(target)]
	if len(matches) == 0 {
		return "", false, ref + " points to a patient that is not in the file"
	}
	if len(matches) > 1 {
		return "", false, fmt.Sprintf("%s matches %d patients when ignoring capital letters", ref, len(matches))
	}
	return matches[0], matches[0] != target, ""
}

// patientLink is one record's link to a patient.
type patientLink struct {
	id  string // the record's id
	raw int64  // its line in raw_resources
	ref string // the link, e.g. "Patient/p1"
}

// linkTable links every record in one table to its patient, logging links
// that don't match or only match by ignoring capital letters.
func (l *Loader) linkTable(table, resourceType string, patients patientIndex) error {
	// find all records in the db
	links, err := l.readLinks(`
SELECT id, raw_resource_id, patient_ref
FROM ` + table + `
WHERE patient_ref IS NOT NULL`)
	if err != nil {
		return err
	}
	for _, link := range links {
		// for a given record, find if it has a real patient
		patientID, caseMismatch, problem := patients.find(link.ref)
		issue := Issue{RawResourceID: link.raw, RecordType: resourceType, RecordID: link.id, Field: "subject.reference", Severity: Warning}

		// patientID == "" if there is no linked patient
		if patientID == "" {
			issue.Type, issue.Detail, issue.ActionTaken = UnknownPatient, problem, "left unlinked"
			if err := l.addIssue(issue); err != nil {
				return err
			}
			continue
		}
		if caseMismatch {
			issue.Type = CaseMismatchReference
			issue.Detail = fmt.Sprintf("%s differs from patient %q only in capital letters", link.ref, patientID)
			issue.ActionTaken = "linked to " + patientID
			if err := l.addIssue(issue); err != nil {
				return err
			}
		}
		if _, err := l.tx.Exec(`
UPDATE `+table+`
SET patient_id = ?
WHERE id = ?`, patientID, link.id); err != nil {
			return err
		}
	}
	return nil
}

// readLinks runs a query returning (record id, raw line, patient link) rows.
// The rows are read in full first, so the query is finished before the
// updates run in the same transaction.
func (l *Loader) readLinks(query string) ([]patientLink, error) {
	rows, err := l.tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var links []patientLink
	for rows.Next() {
		var link patientLink
		if err := rows.Scan(&link.id, &link.raw, &link.ref); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// ---------- Attachments ----------

// checkAttachments checks the links between notes and their files (Binaries).
func (l *Loader) checkAttachments() error {
	// Notes that link to a Binary that isn't in the file. That's why LEFT JOIN on binaries
	if err := l.flagRows(
		`
SELECT a.document_id, d.raw_resource_id, 'Binary/' || a.binary_id || ' is not in the file'
FROM document_attachments a
JOIN document_references d ON d.id = a.document_id
LEFT JOIN binaries b ON b.id = a.binary_id
WHERE b.id IS NULL`,
		Issue{RecordType: "DocumentReference", Type: BinaryBrokenReference, Severity: Warning,
			Field: "content.attachment.url", ActionTaken: "note has no text for this attachment"}); err != nil {
		return err
	}

	// Note and Binary disagree on the content type: trust the Binary.
	if err := l.flagRows(
		`
SELECT a.document_id, d.raw_resource_id,'note says ' || a.content_type || ', Binary/' || a.binary_id || ' says ' || b.content_type
FROM document_attachments a
JOIN document_references d ON d.id = a.document_id
JOIN binaries b ON b.id = a.binary_id
WHERE b.is_inline = 0
AND a.content_type IS NOT NULL
AND a.content_type <> b.content_type`, // disagree
		Issue{RecordType: "DocumentReference", Type: ContentTypeMismatch, Severity: Warning,
			Field: "content.attachment.contentType", ActionTaken: "trusted the Binary's type"}); err != nil {
		return err
	}

	// An orphan Binary is a file that no note points to, so we don’t know whose it is.
	// LEFT JOIN on documents_attachments which won't be found
	return l.flagRows(
		`
SELECT b.id, b.raw_resource_id, 'no DocumentReference links to this Binary, so its patient is unknown'
FROM binaries b
LEFT JOIN document_attachments a ON a.binary_id = b.id
WHERE a.binary_id IS NULL
AND b.is_inline = 0`,
		Issue{RecordType: "Binary", Type: OrphanBinary, Severity: Info, ActionTaken: "kept, not shown on any chart"})
}

// flagRows logs one issue for each row a query finds. The query returns the
// record id, its raw line and the issue's detail; the rest comes from issue.
func (l *Loader) flagRows(query string, issue Issue) error {
	rows, err := l.tx.Query(query)
	if err != nil {
		return err
	}
	var found []Issue
	for rows.Next() {
		if err := rows.Scan(&issue.RecordID, &issue.RawResourceID, &issue.Detail); err != nil {
			rows.Close()
			return err
		}
		found = append(found, issue)
	}
	rows.Close()
	return l.addIssues(found) // saved after the query is finished
}

// ---------- Duplicate and corrected observations ----------

type obsRow struct {
	id, status string
	raw        int64
}

// groupObservations finds lab results that look like the same reading: different
// ids, but the same patient, lab test code and exact time (date-only times are
// never grouped, since two tests on one day can be real). Each group is
// resolved by resolveGroup.

// Try to find observations with different id's but the same results.
// Each Observation is given a label like:
// obs-1	noah|A1c|2024-03-01 09:00
// obs-2	noah|A1c|2024-03-01 09:00
// obs-3	noah|A1c|2024-06-01 09:00
// Group each equivalent Observation together in the GroupsMap
// if a key has more than 1, it means there are duplicates
func (l *Loader) groupObservations() error {
	rows, err := l.tx.Query(`
SELECT o.id, coalesce(o.status, ''), o.raw_resource_id, o.patient_id || '|' || coalesce(o.code_system, '') || '|' || o.lab_test_code || '|' || o.measured_at AS grp
FROM observations o
WHERE o.patient_id IS NOT NULL
AND o.lab_test_code IS NOT NULL
AND o.measured_at LIKE '%T%'
AND coalesce(o.status, '') <> 'entered-in-error'
ORDER BY o.patient_id, o.lab_test_code, o.measured_at, o.code_system, o.id`)
	if err != nil {
		return err
	}
	groups := map[string][]obsRow{} // group key → its readings, sorted by id
	var order []string              // group keys in the order found, so issues come out the same every time
	for rows.Next() {
		var r obsRow
		var grp string
		if err := rows.Scan(&r.id, &r.status, &r.raw, &grp); err != nil {
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
		// if a key has more than 1, it means there are duplicates
		if len(groups[key]) > 1 {
			if err := l.resolveGroup(key, groups[key]); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveGroup decides which reading in a group is shown. Readings are sorted by id.
//   - A correction (status amended or corrected) replaces all the others.
//     If there are several, the lowest id wins.
//   - Otherwise the lowest id is shown, and the others are hidden as duplicates.
//
// Nothing is deleted: hidden readings stay in the table, and /internal/data-quality/issues lists them.
func (l *Loader) resolveGroup(groupKey string, members []obsRow) error {
	keep := members[0]
	for _, m := range members {
		if m.status == "amended" || m.status == "corrected" {
			keep = m
			break
		}
	}
	isCorrection := keep.status == "amended" || keep.status == "corrected"

	var ids []string
	for _, m := range members {
		ids = append(ids, m.id)
	}

	for _, m := range members {
		issue := Issue{RawResourceID: m.raw, RecordType: "Observation", RecordID: m.id, GroupKey: groupKey}
		switch {
		// case (keep.id) in which we are displaying the data so there's no issue
		case isCorrection && m.id == keep.id:
			continue
		// for all correction duplicates, set the replaced_by and is_primary=no
		case isCorrection:
			if _, err := l.tx.Exec(`
UPDATE observations
SET replaced_by = ?, is_primary = 0
WHERE id = ?`, keep.id, m.id); err != nil {
				return err
			}
			issue.Type, issue.Severity, issue.Field = Superseded, Info, "status"
			issue.Detail = fmt.Sprintf("replaced by %s (status %s)", keep.id, keep.status)
			issue.ActionTaken = "hidden by default, kept as history"
		// duplicates that aren't corrections.
		default:
			issue.Type, issue.Severity = DuplicateRecord, Warning
			issue.Detail = "same patient, lab test code and time as " + others(ids, m.id)
			issue.ActionTaken = "primary (lowest id)"
			if m.id != keep.id {
				if _, err := l.tx.Exec(`
UPDATE observations
SET is_primary = 0
WHERE id = ?`, m.id); err != nil {
					return err
				}
				issue.ActionTaken = "hidden by default, primary is " + keep.id
			}
		}
		if err := l.addIssue(issue); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Possible duplicate patients ----------

// flagSameNameAndBirth flags patients with the same name and birth date. Names
// are compared ignoring case, accents and punctuation (name_key). They may be
// the same person from two clinics: kept apart on the chart (a wrong guess would
// show a stranger's records), but counted once in research.
func (l *Loader) flagSameNameAndBirth() error {
	// Each patient p is paired with every other patient q who has the same name
	// and birth date; GROUP BY turns those pairs into one row (one issue) per patient.
	return l.flagRows(
		`
SELECT p.id, p.raw_resource_id,p.full_name || ', born ' || p.birth_date || ', also appears as ' || group_concat(q.id, ', ')
FROM patients p
JOIN patients q ON q.name_key = p.name_key
AND q.birth_date = p.birth_date
AND q.id <> p.id
GROUP BY p.id
ORDER BY p.id`,
		Issue{RecordType: "Patient", Type: PossibleDuplicatePatient, Severity: Warning,
			ActionTaken: "kept separate on the chart, counted once in research"})
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
