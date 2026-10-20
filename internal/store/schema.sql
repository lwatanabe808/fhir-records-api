-- The database is rebuilt from the data file on every start.
-- raw_resources keeps every line exactly as received.
-- The clean tables hold only valid values (invalid ones become NULL).
-- issues explains every problem, and points back to the raw line.

CREATE TABLE raw_resources (
  id            INTEGER PRIMARY KEY,   -- equals the line number in the file
  resource_type TEXT,
  fhir_id       TEXT,
  patient_ref   TEXT,                  -- patient link as written, for any type
  patient_id    TEXT,                  -- resolved patient, set in pass 2
  parsed_ok     INTEGER NOT NULL,
  raw_json      TEXT,                  -- original JSON when it parsed
  raw_text      TEXT                   -- original text when it did not parse
);
CREATE INDEX idx_raw_type_id ON raw_resources(resource_type, fhir_id);
CREATE INDEX idx_raw_patient ON raw_resources(patient_id);

CREATE TABLE patients (
  id              TEXT PRIMARY KEY,
  raw_resource_id INTEGER NOT NULL,
  full_name       TEXT,
  family_name     TEXT,
  given_names     TEXT,
  gender          TEXT,
  birth_date      TEXT,
  deceased_date   TEXT,
  active          INTEGER,
  name_key        TEXT,     -- normalized name, for matching
  ssn_hash        TEXT,     -- keyed hash of a valid SSN; the number itself is never stored here
  person_id       TEXT      -- patients confirmed as the same person share this (set in pass 2)
);
CREATE INDEX idx_patient_ssn ON patients(ssn_hash);
CREATE INDEX idx_patient_person ON patients(person_id);
CREATE INDEX idx_patient_name_birth ON patients(name_key, birth_date);
CREATE INDEX idx_patient_birth ON patients(birth_date);
CREATE INDEX idx_patient_family ON patients(family_name, given_names);

CREATE TABLE observations (
  id              TEXT PRIMARY KEY,
  raw_resource_id INTEGER NOT NULL,
  patient_ref     TEXT,
  patient_id      TEXT,
  code_system     TEXT,
  lab_test_code   TEXT,
  name            TEXT,
  label           TEXT,
  status          TEXT,
  measured_at     TEXT,
  last_updated    TEXT,
  issued          TEXT,
  is_primary      INTEGER NOT NULL DEFAULT 1,  -- 0 when another record in its duplicate group wins
  replaced_by     TEXT,                        -- set when an amended record replaces this one
  is_impossible   INTEGER NOT NULL DEFAULT 0   -- hidden by default
);
-- A patient's results for one test, newest first (patient views, summary, duplicate check).
CREATE INDEX idx_obs_patient_test ON observations(patient_id, lab_test_code, measured_at);
-- Everyone's results for one test (research cohort and lab summary).
CREATE INDEX idx_obs_test_patient ON observations(lab_test_code, patient_id, measured_at);

-- Every result value of an observation, in one place.
--   A single result (A1c 7.8 %): one row, is_component = 0, with the observation's own lab test code.
--   A multi-part result (blood pressure): one row per part (systolic 120, diastolic 80),
--   is_component = 1, each with its own lab test code.
-- So any query for a lab test code finds its numbers here, wherever FHIR put them.
CREATE TABLE observation_values (
  observation_id TEXT NOT NULL,
  is_component   INTEGER NOT NULL,
  code_system    TEXT,
  lab_test_code  TEXT,
  name           TEXT,
  value          REAL,
  value_text     TEXT,
  unit           TEXT
);
CREATE INDEX idx_values_obs ON observation_values(observation_id);
CREATE INDEX idx_values_code ON observation_values(lab_test_code);

CREATE TABLE conditions (
  id                  TEXT PRIMARY KEY,
  raw_resource_id     INTEGER NOT NULL,
  patient_ref         TEXT,
  patient_id          TEXT,
  code_system         TEXT,
  disease_code        TEXT,
  name                TEXT,
  label               TEXT,
  clinical_status     TEXT,
  verification_status TEXT,
  onset_date          TEXT
);
CREATE INDEX idx_cond_patient ON conditions(patient_id);
CREATE INDEX idx_cond_code ON conditions(disease_code, patient_id);

CREATE TABLE medications (
  id              TEXT PRIMARY KEY,
  raw_resource_id INTEGER NOT NULL,
  patient_ref     TEXT,
  patient_id      TEXT,
  code_system     TEXT,
  drug_code       TEXT,
  name            TEXT,
  label           TEXT,
  status          TEXT,
  intent          TEXT,
  prescribed_at   TEXT,
  dosage          TEXT
);
CREATE INDEX idx_med_patient ON medications(patient_id);
CREATE INDEX idx_med_code ON medications(drug_code, patient_id);

CREATE TABLE procedures (
  id              TEXT PRIMARY KEY,
  raw_resource_id INTEGER NOT NULL,
  patient_ref     TEXT,
  patient_id      TEXT,
  code_system     TEXT,
  procedure_code  TEXT,
  name            TEXT,
  label           TEXT,
  status          TEXT,
  performed_at    TEXT,
  performer       TEXT
);
CREATE INDEX idx_proc_patient ON procedures(patient_id);

CREATE TABLE document_references (
  id              TEXT PRIMARY KEY,
  raw_resource_id INTEGER NOT NULL,
  patient_ref     TEXT,
  patient_id      TEXT,
  status          TEXT,
  note_type       TEXT,
  note_type_code  TEXT,
  date            TEXT,
  author          TEXT
);
CREATE INDEX idx_doc_patient ON document_references(patient_id);

-- Binary content. Inline attachments are stored here too, with a generated id.
CREATE TABLE binaries (
  id              TEXT PRIMARY KEY,
  raw_resource_id INTEGER NOT NULL,
  content_type    TEXT NOT NULL,    -- the type we trust (declared, or detected from the bytes)
  size_bytes      INTEGER,
  text            TEXT,             -- decoded text, only for readable text content
  data            BLOB,             -- decoded bytes, NULL when invalid or too large
  is_inline       INTEGER NOT NULL DEFAULT 0
);

-- Many to many: a note can have several attachments, and notes can share a Binary.
CREATE TABLE document_attachments (
  document_id  TEXT NOT NULL,
  binary_id    TEXT NOT NULL,
  content_type TEXT,               -- what the note says the attachment is
  PRIMARY KEY (document_id, binary_id)
);
CREATE INDEX idx_att_binary ON document_attachments(binary_id);

CREATE TABLE issues (
  id              INTEGER PRIMARY KEY,
  raw_resource_id INTEGER,          -- the line the problem is on
  record_type     TEXT,
  record_id       TEXT,
  issue_type      TEXT NOT NULL,
  severity        TEXT NOT NULL,    -- error, warning or info
  field           TEXT,
  detail          TEXT,
  action_taken    TEXT,
  group_key       TEXT              -- ties together records in the same duplicate or correction group
);
CREATE INDEX idx_issue_raw ON issues(raw_resource_id);
CREATE INDEX idx_issue_type ON issues(issue_type);
CREATE INDEX idx_issue_group ON issues(group_key);
CREATE INDEX idx_issue_record ON issues(record_type, record_id);
CREATE INDEX idx_issue_severity ON issues(severity);
