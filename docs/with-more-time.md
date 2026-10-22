# With more time

The full list. The README keeps the most important ones.

- **Note search across all patients** for research. Notes are free text full of names, dates and places, so the text itself would need de-identifying first, which is a hard problem on its own. At that scale it would also need a full-text index (SQLite FTS5), and meaning-based search (so "felt sick" matches "nausea") would need an NLP or embedding model.
- **Medication journeys:** each drug's start, stop, switch and dose changes in order, with the reason from the notes. That is what insurance claims miss and what partners ask about. The sample data has one prescription per drug, so it needs richer test data first.
- **Loading new records as they arrive** instead of rebuilding the database on every start, to keep each patient's record current.
- **Source system per record.** The data doesn't say which clinic each record came from, which matters when a patient's care spans several systems.
- **Exact decimals.** Lab values are stored as floating point numbers, which is fine for filtering and display but drops FHIR's decimal precision (`7.80` vs `7.8`). The original text is kept in `raw_resources`; a clean column for it would be next.
- **Code checks against LOINC and SNOMED** through a terminology server, so a code that doesn't exist gets flagged. Today only the presence of a code and its code system is checked.
- **Searching by code families** using the SNOMED hierarchy, so `condition` can mean "every kind of diabetes mellitus" without matching prediabetes. Today a keyword search is broad and a code search is exact.
- **Fetch web address attachments** from the source system using the patient's portal connection, and store large files in a private, encrypted S3 bucket, served through short-lived pre-signed URLs.
- **More record types,** like Immunization and AllergyIntolerance, each with its own checks and clean table, so they can appear on the patient's chart.
- **A combined timeline** of every record type for a patient, newest first.
- **Searching a patient's note text,** and **research summaries** like patients per condition or lab value ranges across patients. The cohort search covers the counts partners ask for first.
- **Matching people by identifiers** like SSN or medical record number, stored only as a keyed hash, to combine a person's records from several clinics into one chart.
- **Picking the newest of several corrections** by `meta.lastUpdated`, and telling exact copies apart from duplicates with different values.
- **Dates checked against the patient's life:** a measurement dated before the patient was born or after they died.
- **Cross-checks between values,** like a blood pressure whose systolic (top) number isn't higher than its diastolic (bottom) number.
- **Implausible values** (possible, but likely a typo, like an A1c of 78%): ranges per lab test, reviewed by a clinician, or learned from the data with percentiles rather than means, which outliers distort.
- **Code and label checks** using medical synonym data or an LLM that flags suspicious pairs for a person to review.
- **Small-count suppression in cohort results** too (the dataset profile already hides small groups), so a very small cohort can't single out a person.
- **FHIR Bundles** unpacked when a line contains one, and contained resources (`#id` attachments).
- **Converting lab units** to one standard unit per test, so `labMin` and `labMax` compare like with like.
- **Patient links by medical record number** (`subject.identifier`), not just by id.

Back to the [README](../README.md).
