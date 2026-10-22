# Test input files

Besides the sample file, [testdata/scenarios](../testdata/scenarios) has input files that each test one kind of situation. Each one is checked by a test in [internal/loader/scenarios_test.go](../internal/loader/scenarios_test.go). Try any of them with `make run DATA=testdata/scenarios/<file>`.

| File | What it tests | Expected result |
|---|---|---|
| `01-clean-new-patients` | New patients, new lab codes (grip strength), time zones, partial dates, accented names | Everything loads, 0 issues |
| `02-reverse-order` | Notes before their Binary, labs before their patient | All links resolve, 0 issues |
| `03-line-endings` | Windows line endings, blank lines, no final line break | 3 records load, blank lines skipped |
| `04-all-broken` | 11 lines broken in different ways | No crash, one issue per line |
| `05-pretty-printed` | One record spread over many lines | Fragments flagged, records around it load |
| `06-unsupported-types` | Encounter, Immunization, Bundle, a misspelled type | Kept raw, still found by patient |
| `07-patient-links` | Ids that differ only by capital letters, links to groups and web addresses, empty links | Ambiguous links never guessed |
| `08-dates` | Partial dates, time zones, same day with no time, February 30 | Readings with a date and no time not treated as duplicates |
| `09-two-clinics` | Same person from two clinics, repeated and corrected records, `meta.lastUpdated` | Patients never merged, newest version kept |
| `10-attachments` | PDF, HTML, inline data, a Binary shared by two notes, an image labeled as text | Each stored with the right type and text |
| `11-values` | Other units, zero, negative, huge, wrong types, a missing value with a reason, value types we don't store | Impossible hidden, every missing or unstored value logged |
| `12-bulk` | 22,000 records (built by `make scenarios`, not kept in git) | Loads in about 3 seconds |
| `13-identifiers` | Records with SSNs, accented and punctuated names | Same name and birth date flagged, SSNs never returned |

Back to the [README](../README.md).
