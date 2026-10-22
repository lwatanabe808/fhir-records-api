# Validation cases

Each case below is covered by a test in [internal/loader/loader_test.go](../internal/loader/loader_test.go), using planted problems in [testdata/messy.jsonl](../testdata/messy.jsonl).

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
| 19 | Value type we don't store yet (`valueRange`, `valueRatio`, `valueSampledData`, `valueTime`, `valueDateTime`, `valuePeriod`) | Own check | Value null, original kept in `raw_resources` | `UNSUPPORTED_VALUE_TYPE` | info |
| 20 | Number with no unit | Own check | Unit null | `MISSING_UNIT` | warning |
| 21 | Empty list (e.g. dosage) | Own check (the schema allows it) | Stored as null | `EMPTY_LIST` | warning |
| 22 | Impossible value: negative, over 100%, dated in the future | Own check | Hidden from the patient app, excluded from research | `IMPOSSIBLE_VALUE` | error |
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
| 39 | Same type and id, changed | Pass 1 | Later version wins (unless `meta.lastUpdated` says otherwise). Older versions stay in `raw_resources` | `DUPLICATE_ID_CHANGED` | warning |
| 40 | Different ids, same patient, test and exact time | Pass 2 | Lowest id is primary, others hidden (same or different values) | `DUPLICATE_RECORD` | warning |
| 41 | Amended or corrected reading | Pass 2 | Replaces the others in its group (`replaced_by`). Several corrections: the lowest id wins | `SUPERSEDED` | info |
| 42 | Same name and birth date (ignoring case, accents and punctuation), different patient ids | Pass 2 | Separate charts, counted once in research | `POSSIBLE_DUPLICATE_PATIENT` | warning |

**Valid records with special handling (no issue logged):**

| # | Case | How it's known | Handling |
|---|---|---|---|
| 43 | Entered in error | `status: "entered-in-error"` | Hidden from the patient app, excluded from research |
| 44 | Refuted diagnosis | `verificationStatus: "refuted"` | Excluded from research |
| 45 | Code and label name different things (e.g. `proc-ti-001`: bypass code, "Cardiac Catheterization" label) | A decision, not a check | Both stored and returned, searches use the code |

**What the sample file triggers:** `med-nw-003` (capital letters, linked), `obs-nw-006` (unknown patient), `obs-kl-bad-001` (name only), `cond-nw-bad-001` (no disease code), `obs-nw-002` / `obs-nw-dup-001` (A1c 7.8 vs 7.9 at the same time), `obs-ti-003` (replaced by the amended `obs-ti-005`), `med-fd-002` (empty dosage list), `note-robby-001` (`ClinicalNote` is not FHIR).

Every issue is listed by `GET /internal/data-quality/issues`. Back to the [README](../README.md).
