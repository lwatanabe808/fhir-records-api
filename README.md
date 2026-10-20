# Novellia FHIR API

An API that loads clinic data in FHIR format, cleans it, flags what's wrong, and makes it searchable for two kinds of users: patients using the Novellia app, and research partners on the B2B side.

Written in Go, with SQLite as the database. Runs with one command in Docker.

## Quick start

**Docker (recommended):**
```bash
make docker            # builds the image (tests run during the build) and starts it on port 8080
```
or without Make:
```bash
docker build -t novellia-fhir-api .
docker run --rm -p 8080:8080 -v "$(pwd)/db:/app/db" novellia-fhir-api
```

**Without Docker** (needs Go 1.26+):
```bash
make run
```

**See the database:** on every start, the app writes the SQLite file to `db/novellia.db` in the project folder (with `make run` and `make docker`). Open it with the VS Code extension **SQLite Viewer**, DB Browser for SQLite, or:
```bash
sqlite3 -header -column db/novellia.db "SELECT * FROM issues"
```

**Try it:**
```bash
make demo              # calls every endpoint with cURL
curl -s localhost:8080/patients/noah-wyle/observations?code=a1c
```

**Use your own data file:**
```bash
make run DATA=path/to/other.jsonl
docker run --rm -p 8080:8080 -v $(pwd)/other.jsonl:/data/other.jsonl -e DATA_FILE=/data/other.jsonl novellia-fhir-api
```

**Tests:**
```bash
make test
```

### Settings

| Variable | Default | Meaning |
|---|---|---|
| `DATA_FILE` | `data/fhir-resources.jsonl` | The JSONL file to load |
| `PORT` | `8080` | HTTP port |
| `DB_PATH` | `db/novellia.db` (`/app/db/novellia.db` in Docker) | SQLite file, rebuilt on every start. Use `:memory:` to skip the file. |
| `MAX_BINARY_MB` | `5` | Decoded attachments over this size are not stored |
| `MAX_RECORD_MB` | `10` | Non-Binary lines over this size are not processed |
| `PATIENT_ID_SECRET` | dev default | Key for the de-identified research ids. Set it in any real environment. |
| `SSN_SECRET` | dev default | Key used to hash Social Security numbers before they are stored. Set it in any real environment. |
| `MIN_CELL_SIZE` | `5` | Research counts below this show as `<5` (condition counts, lab summaries, dataset profile). The sample has only 16 patients, so most groups are hidden; use `MIN_CELL_SIZE=1` to see every number in a demo. |

## Who uses this API

| User | Needs | Endpoints |
|---|---|---|
| **Patients**, through the Novellia app | See their own records in one place, in plain terms, without duplicates or mistakes | `/patients/...` |
| **Research partners** (life science companies) | Find groups of patients matching criteria, with no identifying details | `/research/...` |
| **Novellia's data team** | See what was wrong with incoming data and what the loader did about it | `/data-quality/...` |

## Endpoints

### Patient app
| Endpoint | What it returns |
|---|---|
| `GET /patients?name=&gender=&bornAfter=&bornBefore=&limit=&offset=` | Patients, searchable and paged |
| `GET /patients/{id}` | One patient. Ids work in any capitalization (FHIR ids are case-insensitive). |
| `GET /patients/{id}/summary` | One call for the app's home screen: active conditions, current medications, the latest result of each test, recent procedures |
| `GET /patients/{id}/conditions?status=` | Diagnoses |
| `GET /patients/{id}/medications?status=` | Prescriptions |
| `GET /patients/{id}/observations?code=&from=&to=` | Labs and vitals. `code` takes a LOINC code or a shortcut like `a1c`, `oxygen`, `blood-pressure`. Blood pressure comes back split into systolic and diastolic. |
| `GET /patients/{id}/procedures` | Procedures |
| `GET /patients/{id}/notes?q=` | Notes with decoded text, or the reason text isn't available. `q` searches this patient's note text. |
| `GET /patients/{id}/timeline` | Every record in one list, newest first |
| `GET /patients/{id}/records?type=` | Original FHIR records of any type, including types without a clean table yet |

Each list also has a single record endpoint, for example `GET /patients/{id}/observations/{recordId}`. It returns the record even if it would be hidden, and a 404 if the record belongs to another patient.

Every list item has a `warnings` field listing problems found in that record. Records that shouldn't count as real data are hidden by default: entered in error, replaced by a correction, non-primary duplicates, and impossible values. Add `includeHidden=true` to see them, with a `hiddenBecause` reason.

### Research partners (B2B)
| Endpoint | What it returns |
|---|---|
| `GET /research/cohort?condition=&medication=&procedure=&lab=&labMin=&labMax=&minAge=&maxAge=&gender=` | Count and de-identified details of matching patients. Exclusion criteria: `excludeCondition=`, `excludeMedication=`, `excludeProcedure=`. The count also reports possible duplicate patients. |
| `GET /research/dataset-profile` | What partners ask before buying data: years of history per patient, completeness, coding standards, quality rates, and age and gender (small groups hidden) |
| `GET /research/conditions` | How many patients have each condition. Possible duplicate patients are counted once, and counts under `MIN_CELL_SIZE` show as `<5`. |
| `GET /research/labs/{code}` | Each patient's latest value for one test: count, min, median, max, per unit. A possible duplicate patient contributes one value, and statistics for groups under `MIN_CELL_SIZE` are hidden. |

### Data quality
| Endpoint | What it returns |
|---|---|
| `GET /data-quality/summary` | Load totals, issue counts by type and severity, and how complete key fields are |
| `GET /data-quality/issues?type=&severity=&recordType=&recordId=&groupKey=&includeRaw=` | Every issue, with the line it came from and what was done |

### Other
| Endpoint | What it returns |
|---|---|
| `GET /fhir/{type}/{id}` | The original record, exactly as received |
| `GET /fhir/{type}/{id}/history` | Every version of a record in file order, marked current, replaced or identical copy |
| `GET /health` | Status and the load summary |

Every error uses the same shape: `{"error": {"code": "INVALID_PARAMETER", "message": "limit: must be a whole number from 1 to 500"}}`. Bad parameters return 400, unknown patients and records return 404.

## The extra capability: cohort search

`/research/cohort` answers the question research partners ask: "how many patients like this do you have, and what does their history look like?" For example, `condition=diabetes&lab=a1c&labMin=7` finds patients with type 2 diabetes whose latest A1c is 7 or higher.

Partners describe the patients they need as **inclusion and exclusion criteria**, so both are supported: `condition=diabetes&excludeCondition=heart failure&procedure=colonoscopy`.

**Honest counts.** The same person can arrive from two clinics under two patient ids. The loader never merges them, so every research count accounts for it: every research count gives three numbers: `countWithoutDuplicates` (the conservative number, possible duplicates counted once), `countIncludingPossibleDuplicates` (every patient id), and `possibleDuplicatePatients` (how many were combined). Condition counts, lab summaries and the dataset profile use the same names with `patients` in place of `count`. In cohort results, each affected patient lists the research ids it may duplicate, and a lab summary takes one value per person. A count a partner can trust matters more than a big one.

**Why this one:** it's how Novellia earns revenue. Clinical trials often struggle to find enough eligible patients, and Novellia's advantage is complete, cleaned history across many providers. This endpoint turns the cleaned data into that answer.

**De-identified by design:**
- No names, patient ids or birth dates. Each patient gets a research id, an HMAC of the patient id with a secret key, so it is stable but can't be reversed.
- Ages over 89 are shown as `90+`, and dates are reduced to the year (from the HIPAA Safe Harbor rules).
- Only trustworthy records count: no non-primary duplicates, corrections replace originals, no impossible values, and no refuted or entered-in-error diagnoses.

### Dataset profile

`/research/dataset-profile` answers the questions partners ask to judge whether data is usable, before any cohort search:

| Question | Where it's answered |
|---|---|
| How long is each patient's history? | `history.yearsOfHistory`: median, min and max across patients |
| How much data per patient? How current? | `history.datedRecordsPerPatient`, `history.yearsSinceLatestRecord` |
| How complete are the key fields? | `completenessPercent`: birth date, gender, linked records, codes, units, dosages, readable notes |
| Is it coded in standard terms? | `codingStandards`: percent LOINC, SNOMED, RxNorm, other, or none, per record type |
| How clean is it? | `quality`: impossible, duplicate or replaced values, failed lines, possible duplicate patients |
| Who is in it? | `population`: age bands and gender, with groups under `MIN_CELL_SIZE` shown as `<5` |

It reports medians, not means, because a few extreme patients would pull a mean off.

## How loading works

The data file is loaded into SQLite at startup, in one transaction, in two passes:

1. **Pass 1, one line at a time.** Parse the line, check it against the official FHIR R4 JSON schema, then apply the rules for its type and insert it. Each line is parsed separately, so one bad line never affects the others.
2. **Pass 2, after every line is in.** Resolve patient links, connect notes to their Binaries, find duplicates and corrections, and find orphans. This has to wait, because a record can link to one that appears later in the file.

Principles:
- **Never crash on bad data.** Every problem becomes a row in the `issues` table. Errors in my own code on an unexpected record are caught and logged as `PROCESSING_ERROR`.
- **Keep everything.** Every line is stored as received in `raw_resources`, and every clean row points back to it with `raw_resource_id`.
- **Clean tables only hold valid values.** An invalid value is stored as null in the clean table, the original is kept in `raw_resources`, and an issue explains the difference.
- **Strict about linking, relaxed about loading.** A record is loaded even if its patient link is broken, but it is never attached to a patient by guessing.
- **A type is supported only if it has rules.** Other valid FHIR types are stored raw and reachable through `/patients/{id}/records`.

## Data model

```
raw_resources ─┬─ patients
 (every line)  ├─ observations ── observation_values   (every result value)
               ├─ conditions
               ├─ medications
               ├─ procedures
               ├─ document_references ── document_attachments ── binaries
               └─ issues (every problem, pointing back to its line)
```

- Every clean table has `raw_resource_id`, the line it came from.
- `observations` has `is_primary` and `replaced_by` for duplicates and corrections, and an `is_impossible` flag.
- `observation_values` holds every result value in one place. A single result like A1c 7.8 % is one row. A multi-part result like blood pressure has one row per part (systolic, diastolic), each with its own lab test code. So any query for a lab test code finds its numbers in one table, wherever FHIR put them. For example, `/research/labs/8480-6` summarizes systolic blood pressure.
- `document_attachments` is a link table because a note can have several attachments and several notes can share one Binary. Inline attachments (base64 inside the note) are stored in `binaries` with a generated id like `docref-001-inline-1`.
- `issues` has a `group_key` that ties together records in the same duplicate or correction group.
- Indexes match how the data is read: a patient's results for one test, everyone's results for one test (research), codes for cohort filters, and issues by record, type, severity and group. Checked with `EXPLAIN QUERY PLAN`. Name search uses `LIKE '%...%'`, which can't use an index; full-text search (SQLite FTS5) would fix that at scale.

The full schema is in [internal/store/schema.sql](internal/store/schema.sql).

## Validation cases

Each case below is covered by a test in [internal/loader/loader_test.go](internal/loader/loader_test.go), using planted problems in [testdata/messy.jsonl](testdata/messy.jsonl).

| # | Case | Caught by | Handling | Issue | Severity |
|---|---|---|---|---|---|
| **Step 1: Parse each line** |||||
| 1 | Invalid JSON (cut off, bad syntax) | Parse | Skip the line, keep going, keep the text | `INVALID_JSON` | error |
| 2 | Blank line | Parse | Skip | none | |
| 3 | Non-Binary line over the size limit | Parse | Store type and id only | `RECORD_TOO_LARGE` | warning |
| **Step 2: FHIR schema check** (official R4 JSON schema, checked against the record's own type) |||||
| 4 | Not a record (`[]`, `{}`, no `resourceType`) | Parse | Raw only | `MALFORMED_RESOURCE` | error |
| 5 | Not a FHIR type (`ClinicalNote`) | Schema type list | Raw only | `NON_STANDARD_TYPE` | warning |
| 6 | Invalid status (`"done"`) | Schema | Insert with status null | `INVALID_STATUS` | warning |
| 7 | Unreadable date (`"2025-13-45"`) | Schema | Insert with date null | `INVALID_DATE` | warning |
| 8 | Wrong value type (`"7.8"` as text) | Schema | Insert with value null | `INVALID_VALUE_TYPE` | warning |
| 9 | Patient link not an object (`"subject": "noah-wyle"`) | Own check (the schema allows it) | Insert unlinked | `INVALID_REFERENCE` | warning |
| 10 | Any other format error (e.g. unknown field) | Schema | Insert, keep the record | `INVALID_FHIR` | warning |
| **Step 3: Rules for each type** |||||
| 11 | Valid FHIR, not supported (e.g. Immunization) | Own check | Raw only, still reachable by patient | `UNSUPPORTED_TYPE` | info |
| 12 | No id | Own check | Raw only | `MISSING_ID` | error |
| 13 | No patient link, or a name only | Own check | Insert unlinked, never matched by name | `NO_PATIENT_LINK` | warning |
| 14 | Patient link without `Patient/` | Own check | Insert unlinked | `INVALID_REFERENCE` | warning |
| 15 | Missing disease, lab test, procedure or drug code | Own check | Code stored as null | `MISSING_CODE` | warning |
| 16 | Code with no code system | Own check | Code system stored as null | `MISSING_CODE_SYSTEM` | warning |
| 17 | Observation with no value | Own check | Value null | `MISSING_VALUE` | warning |
| 18 | More than one value in an Observation | Own check | Keep the first, in a fixed order | `MULTIPLE_VALUES` | warning |
| 19 | Number with no unit | Own check | Unit null | `MISSING_UNIT` | warning |
| 20 | Empty list (e.g. dosage) | Own check (the schema allows it) | Stored as null | `EMPTY_LIST` | warning |
| 21 | Impossible value: negative, over 100%, dated in the future, before birth, after death | Own check | Hidden by default, excluded from research | `IMPOSSIBLE_VALUE` | error |
| 23 | Unexpected error in my code on one record | Recover | Keep going | `PROCESSING_ERROR` | error |
| **Binaries and attachments** |||||
| 24 | Inline attachment (`data` in the note) | Own check | Normal case: stored in `binaries` with a generated id | none | |
| 25 | Not valid base64 | Own check (the schema allows any string) | Data and text null | `INVALID_BINARY` | warning |
| 26 | Over `MAX_BINARY_MB` | Own check | Data not stored | `FILE_TOO_LARGE` | warning |
| 27 | Missing content type | Own check (the schema allows it) | `application/octet-stream` | `MISSING_BINARY_CONTENT_TYPE` | warning |
| 28 | Badly formatted content type (`"pdf"`) | Own check | `application/octet-stream` | `INVALID_CONTENT_TYPE` | warning |
| 29 | Label disagrees with the actual bytes | Own check (byte inspection) | Store the detected type, text null if not text | `CONTENT_TYPE_CONFLICT` | warning |
| 30 | Note and Binary disagree on the type | Pass 2 | Trust the Binary | `CONTENT_TYPE_MISMATCH` | warning |
| 31 | Attachment with no url and no data | Own check | No link | `MISSING_BINARY` | warning |
| 32 | Attachment url in an invalid format | Own check | No link | `INVALID_ATTACHMENT_URL` | warning |
| 33 | Attachment is a web address | Own check | Not fetched | `UNRESOLVABLE_ATTACHMENT` | info |
| **Step 4: After all lines are loaded** |||||
| 34 | Capital letter mismatch (`Patient/Noah-Wyle`) | Pass 2 | Linked only if exactly one patient matches | `CASE_MISMATCH_REFERENCE` | warning |
| 35 | Patient not in the file (`Patient/nwyle`) | Pass 2 | Left unlinked | `UNKNOWN_PATIENT` | warning |
| 36 | Note links to a missing Binary | Pass 2 | Link kept, no text | `BINARY_BROKEN_REFERENCE` | warning |
| 37 | Binary no note links to | Pass 2 | Kept, on no chart | `ORPHAN_BINARY` | info |
| 38 | Same type and id, identical | Pass 1 | Copy skipped | `DUPLICATE_ID` | info |
| 39 | Same type and id, changed | Pass 1 | Later version wins (unless `meta.lastUpdated` says otherwise). Older versions stay in `raw_resources` and are listed by `/fhir/{type}/{id}/history` | `DUPLICATE_ID_CHANGED` | warning |
| 40 | Different ids, same patient, test and exact time, different values | Pass 2 | Lowest id is primary, others hidden | `DUPLICATE_RECORD` | warning |
| 41 | Same as 40, same values | Pass 2 | Lowest id is primary, others hidden | `DUPLICATE_RECORD` | info |
| 42 | Amended or corrected reading | Pass 2 | Replaces the original (`replaced_by`) | `SUPERSEDED` | info |
| 43 | Same name and birth date (ignoring case, accents and punctuation), different patient ids | Pass 2 | Separate charts, counted once in research | `POSSIBLE_DUPLICATE_PATIENT` | warning |
| 43a | Same SSN, and name and birth date agree | Pass 2 | Combined into one chart (shared `person_id`), counted once | `SAME_PERSON_BY_SSN` | info |
| 43b | Same SSN, but name or birth date disagree | Pass 2 | Separate charts, counted once in research | `SSN_CONFLICT` | warning |
| 43c | SSN is a placeholder, fake, partial (last 4) or not 9 digits | Pass 1 | Not used for matching | `INVALID_SSN` | warning |

**Valid records with special handling (no issue logged):**

| # | Case | How it's known | Handling |
|---|---|---|---|
| 44 | Entered in error | `status: "entered-in-error"` | Hidden by default, excluded from research |
| 45 | Refuted diagnosis | `verificationStatus: "refuted"` | Excluded from research |
| 46 | Code and label name different things (e.g. `proc-ti-001`: bypass code, "Cardiac Catheterization" label) | A decision, not a check | Both stored and returned, searches use the code |

**What the sample file triggers:** `med-nw-003` (capital letters, linked), `obs-nw-006` (unknown patient), `obs-kl-bad-001` (name only), `cond-nw-bad-001` (no disease code), `obs-nw-002` / `obs-nw-dup-001` (A1c 7.8 vs 7.9 at the same time), `obs-ti-003` (replaced by the amended `obs-ti-005`), `med-fd-002` (empty dosage list), `note-robby-001` (`ClinicalNote` is not FHIR).

## Test input files

Besides the sample file, [testdata/scenarios](testdata/scenarios) has input files that each test one kind of situation. Each one is checked by a test in [internal/loader/scenarios_test.go](internal/loader/scenarios_test.go). Try any of them with `make run DATA=testdata/scenarios/<file>`.

| File | What it tests | Expected result |
|---|---|---|
| `01-clean-new-patients` | New patients, new lab codes (grip strength), time zones, partial dates, accented names | Everything loads, 0 issues |
| `02-reverse-order` | Notes before their Binary, labs before their patient | All links resolve, 0 issues |
| `03-line-endings` | Windows line endings, blank lines, no final line break | 3 records load, blank lines skipped |
| `04-all-broken` | 11 lines broken in different ways | No crash, one issue per line |
| `05-pretty-printed` | One record spread over many lines | Fragments flagged, records around it load |
| `06-unsupported-types` | Encounter, Immunization, Bundle, a misspelled type | Kept raw, still found by patient |
| `07-patient-links` | Ids that differ only by capital letters, links to groups and web addresses, empty links | Ambiguous links never guessed |
| `08-dates` | Partial dates, time zones, same day with no time, after death, February 30 | Date-only readings not treated as duplicates |
| `09-two-clinics` | Same person from two clinics, repeated and corrected records, `meta.lastUpdated` | Patients never merged, newest version kept |
| `10-attachments` | PDF, HTML, inline data, a Binary shared by two notes, an image labeled as text | Each stored with the right type and text |
| `11-values` | Other units, zero, negative, huge, wrong types, a missing value with a reason | Impossible hidden, every missing value logged |
| `13-identifiers` | Same SSN across clinics, SSN conflict, placeholder, partial and fake SSNs, accented names | One combined chart, one conflict, SSNs never returned |
| `12-bulk` | 22,000 records (built by `make scenarios`, not kept in git) | Loads in about 3 seconds |

## Key decisions and tradeoffs

- **Go, standard library router, SQLite.** Go 1.22+ routing handles path patterns without a framework. SQLite needs no server, and the pure Go driver means the Docker image needs no C compiler.
- **Rebuild the database on every start.** The data file is the source of truth, so the same file always gives the same result. The tradeoff: any manual fixes would be lost on restart.
- **Records decoded as maps, not typed structs.** A typed struct fails the whole record when one field has the wrong type. With maps, each field is read on its own, so the valid parts are kept.
- **The official FHIR schema, plus my own checks.** The schema catches format problems in one step, including ones I didn't think of. But it can't see links between records, and it allows several things FHIR says are wrong (empty lists, any string as base64, a missing Binary content type, a plain string as a patient link), so those are my own checks.
- **Never guess a patient.** Linking by name, or attaching `Patient/nwyle` to Noah, risks putting health data on the wrong person. An unlinked record is visible and fixable; a wrong link is invisible.
- **Capital letters are the one exception,** because the FHIR spec says ids are case-insensitive. Even then it only links when exactly one patient matches.
- **Matching people across clinics.** The patient chart and research counts treat the same evidence differently on purpose:

  | Evidence | Patient chart | Research count |
  |---|---|---|
  | Same SSN, and name and birth date agree | Combined | Once |
  | Same SSN, but name or birth date disagree | Separate, `SSN_CONFLICT` | Once (possible duplicate) |
  | Same name and birth date only | Separate, `POSSIBLE_DUPLICATE_PATIENT` | Once (possible duplicate) |

  The chart only combines on strong evidence, because a wrong merge shows one person's health data to another. Research counts lean the other way, so a person is never counted twice. Every count shows `countWithoutDuplicates`, `countIncludingPossibleDuplicates` and `possibleDuplicatePatients`, and `/patients/{id}` lists `linkedPatientIds`.
- **SSNs are never stored or returned.** The loader keeps only a keyed hash (HMAC with `SSN_SECRET`) for matching. The original record is kept as received, so every endpoint that returns raw JSON replaces the SSN with `[redacted]`.
- **Duplicates are flagged, never deleted.** The clean view shows one primary record with a warning, and the others are one parameter away.
- **Medical meaning is not judged.** The API checks structure and consistency. Whether a label like "History of MI" fits an active status, or whether a code and its label describe the same thing, needs clinical judgment.
- **Logs skip query strings,** because they can contain health details.

## Authentication (not built)

- **Login:** OAuth 2.0 / OpenID Connect. Each request carries a signed token saying who the caller is and their role.
- **Patients** can only reach their own records: the token's patient id must match `{id}`. Mismatches return 404, not 403, so the API doesn't confirm that a record exists.
- **Research partners** use separate credentials scoped to `/research/...` only. They never see `/patients/...` or raw records.
- **Data team** access to `/data-quality/...` and raw records, with every access logged, as HIPAA expects.
- **Later:** caregiver access (a parent for a child), stored as explicit grants.

## A decision I'm not fully satisfied with

> *Draft. Replace with your own choice before recording.*

When two records disagree (A1c 7.8 vs 7.9 at the same time), the lowest id becomes the primary value. It's predictable and the conflict is always visible, but the choice itself is arbitrary. With more time I'd add a review queue where someone on the data team picks the correct value, and keep that decision across reloads instead of rebuilding from the file each time.

## With more time

- **Note search across all patients** for research. Notes are free text full of names, dates and places, so the text itself would need de-identifying first, which is a hard problem on its own. At that scale it would also need a full-text index (SQLite FTS5), and meaning-based search (so "felt sick" matches "nausea") would need an NLP or embedding model. Today, search is per patient, where a plain text match is fast and correct.
- **Medication journeys:** each drug's start, stop, switch and dose changes in order, with the reason from the notes. That is what insurance claims miss and what partners ask about. The sample data has one prescription per drug, so it needs richer test data first.
- **Loading new records as they arrive** instead of rebuilding the database on every start, to keep each patient's record current.
- **Source system per record.** The data doesn't say which clinic each record came from, which matters when a patient's care spans several systems.

- **Exact decimals.** Lab values are stored as floating point numbers, which is fine for filtering and display but drops FHIR's decimal precision (`7.80` vs `7.8`). The original text is kept in `raw_resources`; a clean column for it would be next.
- **Code checks against LOINC and SNOMED** through a terminology server, so a code that doesn't exist gets flagged. Today only the presence of a code and its code system is checked.
- **Fetch web address attachments** from the source system using the patient's portal connection, and store large files in a private, encrypted S3 bucket, served through short-lived pre-signed URLs.
- **Cross-checks between values,** like a blood pressure whose systolic (top) number isn't higher than its diastolic (bottom) number.
- **Implausible values** (possible, but likely a typo, like an A1c of 78%): ranges per lab test, reviewed by a clinician, or learned from the data with percentiles rather than means, which outliers distort.
- **Code and label checks** using medical synonym data or an LLM that flags suspicious pairs for a person to review.
- **Small-count suppression in cohort results** too (condition counts, lab summaries and the dataset profile already hide small groups), so a very small cohort can't single out a person.
- **FHIR Bundles** unpacked when a line contains one, and contained resources (`#id` attachments).
- **A Postman collection** alongside the cURL script.

## Project layout

```
cmd/server/          main: settings, load, serve
internal/fhir/       helpers for reading FHIR JSON
internal/validate/   FHIR schema check and impossible value rules
internal/loader/     the two-pass loader and every validation case
internal/store/      database setup and schema.sql
internal/api/        HTTP endpoints (patients, research, data quality)
data/                the sample data file
testdata/            messy.jsonl with planted problems, and the script that builds it
scripts/demo.sh      cURL calls for every endpoint
```

## AI usage

I used Claude (Anthropic) throughout: to learn FHIR, talk through the edge cases and design decisions, and write and test the code. The decisions in this README are mine, worked out case by case, and I reviewed the code it produced.
