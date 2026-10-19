package loader

// Issue types. Each one matches a row in the validation cases table in the README.
const (
	// Parsing
	InvalidJSON       = "INVALID_JSON"
	RecordTooLarge    = "RECORD_TOO_LARGE"
	ProcessingError   = "PROCESSING_ERROR"
	MalformedResource = "MALFORMED_RESOURCE"
	NonStandardType   = "NON_STANDARD_TYPE"
	UnsupportedType   = "UNSUPPORTED_TYPE"
	MissingID         = "MISSING_ID"

	// Same type and id seen earlier in the file
	DuplicateID        = "DUPLICATE_ID"
	DuplicateIDChanged = "DUPLICATE_ID_CHANGED"

	// Found by the FHIR schema check
	InvalidStatus    = "INVALID_STATUS"
	InvalidDate      = "INVALID_DATE"
	InvalidValueType = "INVALID_VALUE_TYPE"
	InvalidReference = "INVALID_REFERENCE"
	InvalidFHIR      = "INVALID_FHIR"

	// Our own checks on one record
	NoPatientLink     = "NO_PATIENT_LINK"
	MissingCode       = "MISSING_CODE"
	MissingCodeSystem = "MISSING_CODE_SYSTEM"
	MissingValue      = "MISSING_VALUE"
	MultipleValues    = "MULTIPLE_VALUES"
	MissingUnit       = "MISSING_UNIT"
	EmptyList         = "EMPTY_LIST"
	ImpossibleValue   = "IMPOSSIBLE_VALUE"
	ImplausibleValue  = "IMPLAUSIBLE_VALUE"
	InvalidSSN        = "INVALID_SSN"

	// Binaries and attachments
	InvalidBinary            = "INVALID_BINARY"
	FileTooLarge             = "FILE_TOO_LARGE"
	MissingBinaryContentType = "MISSING_BINARY_CONTENT_TYPE"
	InvalidContentType       = "INVALID_CONTENT_TYPE"
	ContentTypeConflict      = "CONTENT_TYPE_CONFLICT"
	ContentTypeMismatch      = "CONTENT_TYPE_MISMATCH"
	MissingBinary            = "MISSING_BINARY"
	InvalidAttachmentURL     = "INVALID_ATTACHMENT_URL"
	UnresolvableAttachment   = "UNRESOLVABLE_ATTACHMENT"

	// Found after everything is loaded (pass 2)
	CaseMismatchReference    = "CASE_MISMATCH_REFERENCE"
	UnknownPatient           = "UNKNOWN_PATIENT"
	BinaryBrokenReference    = "BINARY_BROKEN_REFERENCE"
	OrphanBinary             = "ORPHAN_BINARY"
	DuplicateRecord          = "DUPLICATE_RECORD"
	Superseded               = "SUPERSEDED"
	PossibleDuplicatePatient = "POSSIBLE_DUPLICATE_PATIENT"
	SamePersonBySSN          = "SAME_PERSON_BY_SSN"
	SSNConflict              = "SSN_CONFLICT"
)

// Severity levels.
const (
	Error   = "error"
	Warning = "warning"
	Info    = "info"
)

// Issue is one problem found in the data.
type Issue struct {
	RawResourceID int64
	RecordType    string
	RecordID      string
	Type          string
	Severity      string
	Field         string
	Detail        string
	ActionTaken   string
	GroupKey      string // set for duplicate and correction groups
}
