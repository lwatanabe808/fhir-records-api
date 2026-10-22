# With more time

What I'd build next, and a simple way to do each.

**Linking patients through user accounts.** Patients confirm which clinic records are theirs, so their charts can be combined safely, research counts become exact, and research ids can be random instead of derived from the patient id.
- How: add a `users` table and a `user_patients` table that links each user to their patient ids, one per clinic. Each user gets a random research id at signup.

**Loading new files as they arrive,** instead of rebuilding the database on every start.
- How: keep the database between starts, record each file in a `loads` table, and add or replace records by id instead of starting over.

**Note files in S3.** Today each note's file is decoded and stored in the database.
- How: store the file in a private, encrypted S3 bucket, and keep only its location, type, size and readable text in the database. The API checks who's asking, then returns a short-lived download link.

**Converting lab units,** so `labMin` and `labMax` compare like with like. Today an A1c in mmol/mol (53) passes `labMin=7` as if it were a percent.
- How: pick one standard unit per lab test and convert each value to it when loading, keeping the original. Flag units that can't be converted.

**Smarter code search.** Today only the exact code matches, and we keep one code per record.
- How: store every code a record lists in its own table, so a search matches any of them. Use the SNOMED tree, which goes from general to specific ("diabetes mellitus" has "type 1" and "type 2" under it), so a search for a general code also finds the codes under it, but not prediabetes, which sits elsewhere in the tree.

**De-identified note search** for research. Notes are free text full of names, dates and places.
- How: run each note through a de-identification tool that removes names, dates and places, have a person review a sample, then index the cleaned text for search.

**Implausible values:** results that are possible but likely a typo, like an A1c of 78%.
- How: keep a table of normal ranges per lab test, reviewed by a clinician, and flag values outside it for review without hiding them.

**Source system per record.** The data doesn't say which clinic sent each record.
- How: record the sending clinic with each file, store it on every record, and show it in the patient app and data quality reports.

**More record types,** like Immunization and AllergyIntolerance.
- How: for each type, add a table and a load function with its own checks, the same way the current types work, then a patient endpoint.

Back to the [README](../README.md).
