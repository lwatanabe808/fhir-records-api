package loader

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/lwatanabe808/novellia-fhir-api/internal/fhir"
)

// ---------- DocumentReference ----------

// A DocumentReference is a note's label: whose it is, when, and what kind.
// Its content is in one or more attachments. Each attachment either links to a
// Binary record ("url": "Binary/binary-001") or carries the base64 inline ("data").
func insertDocumentReference(rec *recordCtx) error {
	var d fhir.DocumentReference
	rec.decode(&d)

	patientRef, err := rec.patientRef()
	if err != nil {
		return err
	}
	date, err := rec.date("date", d.Date)
	if err != nil {
		return err
	}
	noteType := d.Type.First()
	status := allowed(d.Status, "current", "superseded", "entered-in-error")
	var author string
	if len(d.Author) > 0 {
		author = d.Author[0].Reference
	}
	if err := rec.l.exec(`INSERT INTO document_references (id, raw_resource_id, patient_ref, status, note_type, note_type_code, date, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.id, rec.line, patientRef, status, noteType.Name(),
		noteType.Code, date, author); err != nil {
		return err
	}

	for i, content := range d.Content {
		att := content.Attachment
		field := fmt.Sprintf("content/%d/attachment", i)
		declared, url := att.ContentType, att.URL

		if rec.flagged[field+"/data"] {
			att.Data = nil // wrong type: Go's decoder may have left "" here
		}
		switch {
		case att.Data != nil:
			// Inline content: stored in binaries like any other Binary, with a generated id.
			inlineID := fmt.Sprintf("%s-inline-%d", rec.id, i+1)
			if err := rec.storeBinary(inlineID, declared, *att.Data, true); err != nil {
				return err
			}
			if err := linkAttachment(rec, inlineID, declared); err != nil {
				return err
			}
		case url != "":
			refType, binaryID, ok := fhir.ParseRef(url)
			switch {
			case ok && refType == "Binary":
				// Whether this Binary exists is checked in pass 2.
				if err := linkAttachment(rec, binaryID, declared); err != nil {
					return err
				}
			case strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://"):
				if err := rec.issue(UnresolvableAttachment, Info, field+"/url", url,
					"not fetched; a real loader would download it from the source system"); err != nil {
					return err
				}
			case strings.HasPrefix(url, "#"):
				if err := rec.issue(UnresolvableAttachment, Info, field+"/url", url+" (contained resources are not supported yet)", "not linked"); err != nil {
					return err
				}
			default:
				if err := rec.issue(InvalidAttachmentURL, Warning, field+"/url",
					fmt.Sprintf("%q is not a Binary link or a web address", url), "not linked"); err != nil {
					return err
				}
			}
		default:
			if err := rec.issue(MissingBinary, Warning, field, "attachment has neither url nor data", "note has no content"); err != nil {
				return err
			}
		}
	}
	return nil
}

func linkAttachment(rec *recordCtx, binaryID, declaredType string) error {
	return rec.l.exec(`INSERT OR IGNORE INTO document_attachments (document_id, binary_id, content_type) VALUES (?, ?, ?)`,
		rec.id, binaryID, baseType(declaredType))
}

// ---------- Binary ----------

func insertBinaryResource(rec *recordCtx) error {
	var b fhir.Binary
	rec.decode(&b)
	return rec.storeBinary(rec.id, b.ContentType, b.Data, false)
}

// storeBinary checks and decodes base64 content and stores it in binaries.
// It is used for Binary records and for inline attachments.
func (rec *recordCtx) storeBinary(id, declaredType, data string, inline bool) error {
	field := "data"
	if inline {
		field = "attachment.data"
	}

	// Content type: missing or badly formatted means "unknown file".
	contentType := baseType(declaredType)
	labeled := contentType != "" // only a real label can conflict with the bytes
	switch {
	case declaredType == "":
		contentType = "application/octet-stream"
		if err := rec.issue(MissingBinaryContentType, Warning, "contentType", "no content type for "+id, "treated as application/octet-stream"); err != nil {
			return err
		}
	case contentType == "":
		contentType = "application/octet-stream"
		if err := rec.issue(InvalidContentType, Warning, "contentType", fmt.Sprintf("%q is not a valid content type", declaredType), "treated as application/octet-stream"); err != nil {
			return err
		}
	}

	var decoded []byte
	var size, text any
	if data != "" {
		// Strict base64: Go's decoder rejects bad characters instead of skipping them.
		b, err := base64.StdEncoding.Strict().DecodeString(strings.Join(strings.Fields(data), ""))
		detected := baseType(http.DetectContentType(b))
		switch {
		case err != nil:
			if err := rec.issue(InvalidBinary, Warning, field, "data is not valid base64: "+err.Error(), "data and text stored as null"); err != nil {
				return err
			}
		case len(b) > rec.l.cfg.MaxBinaryBytes:
			size = len(b)
			if err := rec.issue(FileTooLarge, Warning, field,
				fmt.Sprintf("%d bytes is over the %d byte limit", len(b), rec.l.cfg.MaxBinaryBytes), "data not stored"); err != nil {
				return err
			}
		// From here on the data is stored. Check its label against the actual bytes.
		case !labeled:
			size, decoded = len(b), b // no usable label: stays application/octet-stream, as reported above
		case isTextual(contentType) && utf8.Valid(b):
			size, decoded, text = len(b), b, string(b)
		case isTextual(contentType):
			size, decoded = len(b), b
			if err := rec.issue(ContentTypeConflict, Warning, "contentType",
				fmt.Sprintf("labeled %s, but the data is not readable text (looks like %s)", contentType, detected), "text stored as null, stored detected type"); err != nil {
				return err
			}
			contentType = detected
		default:
			size, decoded = len(b), b
			if detected != "application/octet-stream" && detected != contentType {
				if err := rec.issue(ContentTypeConflict, Warning, "contentType",
					fmt.Sprintf("labeled %s, but the data looks like %s", contentType, detected), "stored detected type"); err != nil {
					return err
				}
				contentType = detected
			}
		}
	}
	return rec.l.exec(`INSERT INTO binaries (id, raw_resource_id, content_type, size_bytes, text, data, is_inline)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, rec.line, contentType, size, text, decoded, inline)
}

// baseType returns "text/plain" for "text/plain; charset=utf-8", or "" if invalid.
func baseType(contentType string) string {
	t, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.Contains(t, "/") {
		return ""
	}
	return strings.ToLower(t)
}

// isTextual reports whether a content type should decode to readable text.
func isTextual(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") ||
		strings.HasSuffix(contentType, "/json") || strings.HasSuffix(contentType, "+json") ||
		strings.HasSuffix(contentType, "/xml") || strings.HasSuffix(contentType, "+xml")
}
