# Novellia FHIR API

An API that loads clinic data in FHIR format, cleans it, flags what's wrong, and makes it searchable for patients using the Novellia app, research partners on the B2B side, and Novellia's own staff.

Written in Go, with SQLite as the database. Runs with one command in Docker.

## Quick start

```bash
make docker            # builds the image (tests run during the build) and starts it on port 8080
make run               # or without Docker (needs Go 1.26+)
make demo              # calls every endpoint with cURL
make test              # runs all tests
```

**Postman:** import [postman/novellia-fhir-api.postman_collection.json](postman/novellia-fhir-api.postman_collection.json) to click through every endpoint. Optional filters are listed under each request's Params.

**Your own data file:** `make run DATA=path/to/other.jsonl`

**See the database:** each start writes the SQLite file to `db/novellia.db`. Open it with the VS Code extension SQLite Viewer, or `sqlite3 -header -column db/novellia.db "SELECT * FROM issues"`.

### Settings

| Variable | Default | Meaning |
|---|---|---|
| `DATA_FILE` | `data/fhir-resources.jsonl` | The JSONL file to load |
| `PORT` | `8080` | HTTP port |
| `DB_PATH` | `db/novellia.db` | SQLite file, rebuilt on every start. `:memory:` skips the file. |
| `PATIENT_ID_SECRET` | dev default | Key for the de-identified research ids. Set it in any real environment. |
| `MIN_CELL_SIZE` | `5` | Dataset profile groups below this show as `<5`. Use `1` to see every number in a demo. |
| `MAX_BINARY_MB`, `MAX_RECORD_MB` | `5`, `10` | Size limits for attachments and for other lines |

## Who uses this API

| User | Needs | Endpoints |
|---|---|---|
| **Patients**, through the Novellia app | Their own records in one place, without duplicates or mistakes | `/patients/{id}/...` |
| **Research partners** (life science companies) | Groups of patients matching study criteria, with no identifying details | `/research/...` |
| **Novellia staff** (data team, support) | Look up patients, see what was wrong with incoming data, read original records | `/internal/...` |

Each user has its own path prefix, so access can be controlled one rule per prefix.

## Endpoints

### Patient app
| Endpoint | What it returns |
|---|---|
| `GET /patients/{id}` | One patient, with counts of each record type. Ids work in any capitalization. |
| `GET /patients/{id}/conditions?status=` | Diagnoses |
| `GET /patients/{id}/medications?status=` | Prescriptions |
| `GET /patients/{id}/observations?code=&from=&to=` | Labs and vitals. `code` takes a LOINC code, like `4548-4` for A1c. Blood pressure comes back split into systolic and diastolic. |
| `GET /patients/{id}/procedures` | Procedures |
| `GET /patients/{id}/notes` | Notes with their attachments and decoded text |

These return clean data only: nothing entered in error, replaced by a correction, duplicated or impossible. Hidden records stay in the database. The status values each record type can have are in [docs/status-values.md](docs/status-values.md).

### Research partners (B2B)
| Endpoint | What it returns |
|---|---|
| `GET /research/cohort` | Count and de-identified details of patients matching the filters below |
| `GET /research/dataset-profile` | How many patients, how long and full their histories are, and their age and gender |

### Internal (Novellia staff only)
| Endpoint | What it returns |
|---|---|
| `GET /internal/patients?name=&limit=&offset=` | Patients by name, so support can look someone up |
| `GET /internal/data-quality/summary` | Records by type, how complete key fields are, and issue counts |
| `GET /internal/data-quality/issues?type=&severity=&recordType=&recordId=&groupKey=&includeRaw=` | Every problem found while loading, with its file line and what was done |
| `GET /internal/fhir/{type}/{id}` | The original record, exactly as received (SSNs masked) |

`GET /health` returns the status and load summary. Errors share one shape: `{"error": {"code": "INVALID_PARAMETER", "message": "..."}}`, with 400 for bad parameters and 404 for unknown records.

## The extra capability: cohort search

`/research/cohort` answers the question research partners ask first: "how many patients like this do you have?" For example, `condition=44054006&lab=4548-4&labMin=7` finds patients with type 2 diabetes whose latest A1c is 7 or higher.

**Why this one:** it's how Novellia earns revenue. Clinical trials struggle to find eligible patients, and Novellia's advantage is complete, cleaned history across many providers.

**Filters.** Inclusion and exclusion criteria, by standard medical code or by keyword in the name:

| Filter | Code system | Example code | Example keyword |
|---|---|---|---|
| `condition`, `excludeCondition` | SNOMED CT | `44054006` (type 2 diabetes) | `diabetes` |
| `medication`, `excludeMedication` | RxNorm | `860975` (Metformin 500 MG) | `metformin` |
| `procedure`, `excludeProcedure` | SNOMED CT | `410188000` (diabetic eye exam) | `eye exam` |
| `lab`, with `labMin` and `labMax` | LOINC | `4548-4` (A1c) | codes only |

Plus `minAge`, `maxAge`, `gender`, `limit` and `offset`. Codes are exact; keywords are broad (`diabetes` also matches "Prediabetes"). When a clinic lists its own code before the standard one, the loader keeps the standard code. Lab filters use each patient's latest final result.

**Trustworthy counts.**
- Only clean records count: duplicates, replaced results, mistakes and ruled-out diagnoses are left out.
- The same person can arrive from two clinics under two ids. They're never merged, so each count is given twice: `countWithoutDuplicates` (possible duplicates counted once, the number to use) and `countIncludingPossibleDuplicates`. Each patient lists the research ids it may duplicate in `possibleDuplicateOf`.

**De-identified (HIPAA Safe Harbor).**
- No names, patient ids or birth dates. Each patient gets a stable research id, an HMAC of the patient id with a secret key.
- Ages over 89 show as `90+`, and dates are reduced to the year.

The **dataset profile** answers whether the data is worth buying: history length, records per patient and how recent, using the same clean rules. Age and gender groups smaller than `MIN_CELL_SIZE` show as `<5`.

## How loading works

The file is loaded into SQLite at startup, in two passes:

1. **Pass 1, one line at a time.** Check the line against the official FHIR R4 schema, apply the rules for its type, and save it. One bad line never affects the others.
2. **Pass 2, after every line is in.** Link records to patients, connect notes to their files, and find duplicates and corrections. This waits because a record can point to one later in the file.

Principles:
- **Never crash on bad data.** Every problem becomes a row in `issues`, with what was done about it.
- **Keep everything.** Every line is stored as received in `raw_resources`. Invalid values become null in the clean tables, with the original kept.
- **Never guess a patient.** A broken link leaves the record unlinked. Only differences in capital letters are matched, since FHIR ids are case-insensitive, and only when exactly one patient fits.
- **Flag, don't delete.** Duplicates and corrections are marked and hidden, not removed.
- **Prefer the standard code.** Lab tests, observation components, conditions, medications and procedures can each list several codes for the same thing: the clinic's internal code, a billing code, and often the standard code. The loader keeps the standard one (LOINC for labs, SNOMED or ICD-10 for conditions, RxNorm for drugs, SNOMED or CPT for procedures), or the first code if there's no standard one.

Every case the loader handles, and the test that covers it, is in [docs/validation-cases.md](docs/validation-cases.md). The test input files are described in [docs/test-files.md](docs/test-files.md).

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
- `observations` has `is_primary` and `replaced_by` for duplicates and corrections, and `is_impossible`.
- `observation_values` holds every result value: a single result is one row, and blood pressure is one row per part. So a search by lab test code finds the number wherever FHIR put it, like `lab=8480-6` for systolic.
- `binaries` holds note files, whether they came as separate Binary records or inline in the note. `document_attachments` links notes to files.

The full schema is in [internal/store/schema.sql](internal/store/schema.sql).

## Key decisions and tradeoffs

- **Rebuild the database on every start.** The file is the source of truth, so the same file always gives the same result. The tradeoff: a second file replaces the first, and startup grows with file size.
- **The official FHIR schema, plus my own checks.** The schema catches format problems, including ones I didn't think of. My checks cover what it can't see, like links between records.
- **Separate charts, single counts.** A wrong merge would show one person's health data to another, so possible duplicate patients are never combined on a chart. In research they count once.
- **Medical meaning isn't judged.** The API checks structure and consistency. Whether a diagnosis fits a label needs clinical judgment.
- **Privacy.** SSNs are never used and are masked in raw output. Logs skip query strings, which can contain health details.

## Known limits

Found by stress testing with tricky records and up to 100,000 patients:
- **Units aren't compared.** An A1c in mmol/mol (53) passes `labMin=7` as if it were a percent.
- **Name matching is exact.** "Kathy" and "Katherine" with the same birth date aren't flagged; two different people with the same name and birth date are.
- **Patient links by medical record number, FHIR Bundles, and drugs given by reference** aren't read. Each is logged as an issue and kept raw.
- **Scale:** 100,000 patients take about 44 seconds to load and 1.3 seconds for a lab search, both growing in a straight line.

## Authentication (not built)

- **Login:** OAuth 2.0 / OpenID Connect, with a signed token saying who the caller is and their role.
- **Patients** reach only their own records. Mismatches return 404, so the API doesn't confirm a record exists.
- **Research partners** reach only `/research/...`.
- **Staff** reach `/internal/...`, with every access logged, as HIPAA expects.

## A decision I'm not fully satisfied with

> *Draft. Replace with your own choice before recording.*

When two records disagree (A1c 7.8 vs 7.9 at the same time), the lowest id becomes the primary value. It's predictable and the conflict is always visible, but the choice itself is arbitrary. With more time I'd add a review queue where someone on the data team picks the correct value, and keep that decision across reloads instead of rebuilding from the file each time.

## With more time

- **Loading new files as they arrive,** with a load id per file, instead of rebuilding on every start.
- **Converting lab units** to one standard unit per test, so `labMin` and `labMax` compare like with like.
- **Searching by code families** using the SNOMED hierarchy, so "diabetes" means every kind of diabetes but not prediabetes.
- **Keeping every code, not just one.** Lab tests, components, conditions and their statuses, medications, procedures and note types can each list several codes. Today we keep one, the standard code or else the first. Storing every code in its own row would let a search match any of them, including a clinic's own codes.
- **De-identified note search** for research, which needs the free text cleaned of names and places first.
- **More record types,** like Immunization and AllergyIntolerance.

The full list is in [docs/with-more-time.md](docs/with-more-time.md).

## Project layout

```
cmd/server/          main: settings, load, serve
internal/fhir/       FHIR record types and helpers
internal/validate/   FHIR schema check and impossible value rules
internal/loader/     the two-pass loader and every validation case
internal/store/      database setup and schema.sql
internal/api/        HTTP endpoints (patients, research, internal)
data/                the sample data file
testdata/            files with planted problems, and the scripts that build them
docs/                validation cases, test files, status values, future work
scripts/demo.sh      cURL calls for every endpoint
postman/             Postman collection for every endpoint
```

## Tools and AI usage

- **Go,** because it's what I know best. Novellia works mainly in Node.js; the design would carry over directly.
- **net/http** from the standard library. Go 1.22+ routing handles `/patients/{id}` without a framework.
- **[modernc.org/sqlite](https://gitlab.com/cznic/sqlite):** SQLite in pure Go, so the Docker image needs no C compiler.
- **[santhosh-tekuri/jsonschema](https://github.com/santhosh-tekuri/jsonschema):** checks records against the official FHIR R4 schema.
- **golang.org/x/text:** removes accents from names, so "García" and "Garcia" match when flagging possible duplicates.
- **Docker, Make, cURL and Postman** to run and demo it.

I used Claude (Anthropic) throughout: to learn FHIR, talk through edge cases and design decisions, and write and test the code. The decisions in this README are mine, worked out case by case, and I reviewed the code it produced.
