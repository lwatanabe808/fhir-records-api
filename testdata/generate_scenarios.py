"""Builds testdata/scenarios/*.jsonl: input files that each test one kind of
situation the sample file doesn't cover. Run from the repo root:
    python3 testdata/generate_scenarios.py
Then try one with: make run DATA=testdata/scenarios/<name>.jsonl
"""
import base64
import json
import os

OUT = "testdata/scenarios"
os.makedirs(OUT, exist_ok=True)


def b64(b):
    return base64.b64encode(b).decode()


def loinc(code, display):
    return {"coding": [{"system": "http://loinc.org", "code": code, "display": display}]}


def snomed(code, display):
    return {"coding": [{"system": "http://snomed.info/sct", "code": code, "display": display}]}


def patient(pid, given, family, birth, gender="female", **extra):
    p = {"resourceType": "Patient", "id": pid, "name": [{"family": family, "given": given}], "gender": gender, "birthDate": birth}
    p.update(extra)
    return p


def obs(oid, pid, code, t, value=None, unit=None, status="final", **extra):
    o = {"resourceType": "Observation", "id": oid, "status": status, "code": code,
         "subject": {"reference": "Patient/" + pid}, "effectiveDateTime": t}
    if value is not None:
        o["valueQuantity"] = {"value": value, "unit": unit, "system": "http://unitsofmeasure.org", "code": unit}
    o.update(extra)
    return o


def write(name, records, sep="\n", end="\n"):
    lines = [r if isinstance(r, str) else json.dumps(r) for r in records]
    with open(os.path.join(OUT, name), "w", newline="") as f:
        f.write(sep.join(lines) + (end if lines else ""))


A1C = loinc("4548-4", "Hemoglobin A1c")
GLUCOSE = loinc("2339-0", "Glucose")
WEIGHT = loinc("29463-7", "Body weight")
BP = loinc("85354-9", "Blood pressure panel")
DIABETES = snomed("44054006", "Type 2 diabetes mellitus")


# 01. A clean file with new patients, new codes and every supported type.
#     Expect: everything loads, zero issues.
write("01-clean-new-patients.jsonl", [
    patient("pt-100", ["Maria", "Elena"], "García", "1975-03-02"),
    patient("pt-101", ["James"], "O'Neil", "1960-11-30", gender="male"),
    obs("ob-100", "pt-100", A1C, "2025-06-01T09:00:00Z", 6.9, "%"),
    obs("ob-101", "pt-100", loinc("83174-4", "Grip strength"), "2025-06-01T09:10:00Z", 24.5, "kg"),
    obs("ob-102", "pt-101", BP, "2025-06-02T10:00:00+02:00", component=[
        {"code": loinc("8480-6", "Systolic blood pressure"), "valueQuantity": {"value": 132, "unit": "mmHg"}},
        {"code": loinc("8462-4", "Diastolic blood pressure"), "valueQuantity": {"value": 84, "unit": "mmHg"}}]),
    obs("ob-103", "pt-101", loinc("72166-2", "Tobacco smoking status"), "2025-06-02T10:05:00Z",
        valueCodeableConcept=snomed("266919005", "Never smoked tobacco")),
    {"resourceType": "Condition", "id": "co-100", "clinicalStatus": {"coding": [{"code": "active"}]},
     "verificationStatus": {"coding": [{"code": "confirmed"}]}, "code": DIABETES,
     "subject": {"reference": "Patient/pt-100"}, "onsetDateTime": "2019"},
    {"resourceType": "MedicationRequest", "id": "mr-100", "status": "active", "intent": "order",
     "medicationCodeableConcept": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "860975", "display": "Metformin 500 MG"}]},
     "subject": {"reference": "Patient/pt-100"}, "authoredOn": "2019-04-01", "dosageInstruction": [{"text": "Twice daily"}]},
    {"resourceType": "Procedure", "id": "pr-100", "status": "completed", "code": snomed("73761001", "Colonoscopy"),
     "subject": {"reference": "Patient/pt-101"}, "performedPeriod": {"start": "2024-02-10T08:00:00Z", "end": "2024-02-10T09:00:00Z"}},
    {"resourceType": "Binary", "id": "bn-100", "contentType": "text/plain", "data": b64("Señora García feels well. ✓".encode())},
    {"resourceType": "DocumentReference", "id": "dr-100", "status": "current", "type": loinc("11488-4", "Consult note"),
     "subject": {"reference": "Patient/pt-100"}, "date": "2025-06-01T09:30:00Z",
     "content": [{"attachment": {"contentType": "text/plain", "url": "Binary/bn-100"}}]},
])

# 02. Records in reverse order: notes before binaries, labs before patients.
#     Expect: every link still resolves (pass 2 runs after everything is loaded).
write("02-reverse-order.jsonl", [
    {"resourceType": "DocumentReference", "id": "dr-200", "status": "current", "subject": {"reference": "Patient/pt-200"},
     "date": "2025-01-01T00:00:00Z", "content": [{"attachment": {"contentType": "text/plain", "url": "Binary/bn-200"}}]},
    obs("ob-200", "pt-200", A1C, "2025-01-01T08:00:00Z", 7.1, "%"),
    {"resourceType": "Binary", "id": "bn-200", "contentType": "text/plain", "data": b64(b"Written before the patient line.")},
    patient("pt-200", ["Ada"], "Lovelace", "1985-12-10"),
])

# 03. Windows line endings, blank lines, and no line break at the end.
#     Expect: 3 records load, blank lines skipped, no issues.
write("03-line-endings.jsonl", [
    patient("pt-300", ["Win"], "Dows", "1990-01-01"),
    "",
    obs("ob-300", "pt-300", A1C, "2025-01-01T08:00:00Z", 5.5, "%"),
    "   ",
    obs("ob-301", "pt-300", A1C, "2025-02-01T08:00:00Z", 5.6, "%"),
], sep="\r\n", end="")

# 04. Every line is broken in a different way. Expect: no crash, one issue per line.
write("04-all-broken.jsonl", [
    '{"resourceType":"Patient","id":"cut-off"',
    "{'resourceType': 'Patient'}",
    '{"resourceType":"Patient","id":"trailing-comma",}',
    "null",
    "42",
    '"just a string"',
    "[]",
    "{}",
    '{"resourceType": 7, "id": "number-type"}',
    '{"resourceType": "", "id": "empty-type"}',
    "not json at all",
])

# 05. One record pretty-printed over several lines. Expect: each fragment is
#     INVALID_JSON, and the records around it still load.
pretty = json.dumps(obs("ob-500", "pt-500", A1C, "2025-01-01T08:00:00Z", 6.0, "%"), indent=2).split("\n")
write("05-pretty-printed.jsonl", [patient("pt-500", ["Pretty"], "Print", "1970-01-01")] + pretty +
      [obs("ob-501", "pt-500", A1C, "2025-02-01T08:00:00Z", 6.1, "%")])

# 06. Valid FHIR types this API doesn't have tables for, plus a Bundle.
#     Expect: UNSUPPORTED_TYPE, still reachable under /patients/pt-600/records.
write("06-unsupported-types.jsonl", [
    patient("pt-600", ["Una"], "Supported", "1988-08-08"),
    {"resourceType": "Encounter", "id": "en-600", "status": "finished", "class": {"code": "AMB"}, "subject": {"reference": "Patient/pt-600"}},
    {"resourceType": "AllergyIntolerance", "id": "al-600", "patient": {"reference": "Patient/pt-600"}, "code": {"text": "Penicillin"}},
    {"resourceType": "Immunization", "id": "im-600", "status": "completed", "vaccineCode": {"text": "Flu"},
     "patient": {"reference": "Patient/pt-600"}, "occurrenceDateTime": "2024-10-01"},
    {"resourceType": "Practitioner", "id": "dr-who", "name": [{"family": "Who"}]},
    {"resourceType": "Bundle", "id": "bu-600", "type": "collection", "entry": [{"resource": obs("ob-600", "pt-600", A1C, "2025-01-01T08:00:00Z", 6.0, "%")}]},
    {"resourceType": "Observaton", "id": "typo-600", "subject": {"reference": "Patient/pt-600"}},
])

# 07. Patient id trouble: two patients whose ids differ only by capital letters,
#     links in odd formats, and links to non-patients.
#     Expect: ambiguous capital-letter links stay unlinked, never guessed.
write("07-patient-links.jsonl", [
    patient("abc", ["Lower"], "Case", "1980-01-01"),
    patient("ABC", ["Upper"], "Case", "1981-01-01"),
    patient("pt-700", ["Solo"], "Patient", "1982-01-01"),
    obs("ob-700", "abc", A1C, "2025-01-01T08:00:00Z", 6.0, "%"),                      # exact
    obs("ob-701", "Abc", A1C, "2025-01-02T08:00:00Z", 6.0, "%"),                      # ambiguous: abc or ABC
    obs("ob-702", "PT-700", A1C, "2025-01-03T08:00:00Z", 6.0, "%"),                   # capital letters, one match
    {**obs("ob-703", "x", A1C, "2025-01-04T08:00:00Z", 6.0, "%"), "subject": {"reference": "Group/cohort-1"}},
    {**obs("ob-704", "x", A1C, "2025-01-05T08:00:00Z", 6.0, "%"), "subject": {"reference": "https://ehr.example.com/Patient/pt-700"}},
    {**obs("ob-705", "x", A1C, "2025-01-06T08:00:00Z", 6.0, "%"), "subject": {"reference": ""}},
    {**obs("ob-706", "x", A1C, "2025-01-07T08:00:00Z", 6.0, "%"), "subject": None},
])

# 08. Dates: partial dates, time zones, date-only duplicates, after death.
#     Expect: date-only readings on the same day are NOT grouped as duplicates;
#     the reading after death is impossible.
write("08-dates.jsonl", [
    patient("pt-800", ["Tim"], "Zone", "1950-05", deceasedDateTime="2024-03-01T00:00:00Z"),
    obs("ob-800", "pt-800", WEIGHT, "2020", 70, "kg"),
    obs("ob-801", "pt-800", WEIGHT, "2021-07", 71, "kg"),
    obs("ob-802", "pt-800", GLUCOSE, "2022-01-01", 100, "mg/dL"),
    obs("ob-803", "pt-800", GLUCOSE, "2022-01-01", 180, "mg/dL"),                     # same day, no time: not a duplicate
    obs("ob-804", "pt-800", A1C, "2023-05-05T10:00:00-07:00", 6.0, "%"),
    obs("ob-805", "pt-800", A1C, "2023-05-05T17:00:00Z", 6.4, "%"),                   # same moment in UTC: duplicate
    obs("ob-806", "pt-800", A1C, "2025-01-01T08:00:00Z", 6.2, "%"),                   # after death: impossible
    obs("ob-807", "pt-800", A1C, "2023-02-30T08:00:00Z", 6.2, "%"),                   # Feb 30: invalid date
])

# 09. Same person from two clinics, plus duplicate and changed records.
write("09-two-clinics.jsonl", [
    patient("clinic-a-1", ["Rosa"], "Parks", "1955-02-04"),
    patient("clinic-b-77", ["Rosa"], "Parks", "1955-02-04"),                         # possible duplicate patient
    obs("a-ob-1", "clinic-a-1", A1C, "2025-03-03T08:00:00Z", 7.2, "%"),
    obs("b-ob-9", "clinic-a-1", A1C, "2025-03-03T08:00:00Z", 7.2, "%"),              # same values: info
    obs("a-ob-2", "clinic-a-1", A1C, "2025-04-04T08:00:00Z", 7.0, "%"),
    obs("a-ob-2", "clinic-a-1", A1C, "2025-04-04T08:00:00Z", 7.0, "%"),              # identical copy
    {**obs("a-ob-3", "clinic-a-1", A1C, "2025-05-05T08:00:00Z", 6.8, "%"), "meta": {"lastUpdated": "2025-05-06T00:00:00Z"}},
    {**obs("a-ob-3", "clinic-a-1", A1C, "2025-05-05T08:00:00Z", 6.6, "%"), "meta": {"lastUpdated": "2025-05-05T12:00:00Z"}},  # older version: earlier one kept
    obs("a-ob-4", "clinic-a-1", A1C, "2025-06-06T08:00:00Z", 6.5, "%"),
    obs("a-ob-5", "clinic-a-1", A1C, "2025-06-06T08:00:00Z", 6.7, "%", status="corrected"),   # correction replaces a-ob-4
])

# 10. Attachments: PDF, HTML, inline data, a Binary shared by two notes,
#     several attachments on one note, and an image labeled as text.
png = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
pdf = b"%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n"
write("10-attachments.jsonl", [
    patient("pt-1000", ["Doc"], "Ument", "1977-07-07"),
    {"resourceType": "Binary", "id": "bn-pdf", "contentType": "application/pdf", "data": b64(pdf)},
    {"resourceType": "Binary", "id": "bn-html", "contentType": "text/html", "data": b64(b"<p>Follow up in <b>2 weeks</b>.</p>")},
    {"resourceType": "Binary", "id": "bn-shared", "contentType": "text/plain", "data": b64(b"Shared lab report text.")},
    {"resourceType": "Binary", "id": "bn-fake-text", "contentType": "text/plain; charset=utf-8", "data": b64(png)},
    {"resourceType": "Binary", "id": "bn-spaces", "contentType": "text/plain", "data": "U3Bh Y2Vz IGlu\nIGJh c2U2NA=="},
    {"resourceType": "DocumentReference", "id": "dr-1000", "status": "current", "type": loinc("11506-3", "Progress note"),
     "subject": {"reference": "Patient/pt-1000"}, "date": "2025-01-01T00:00:00Z", "content": [
         {"attachment": {"contentType": "application/pdf", "url": "Binary/bn-pdf"}},
         {"attachment": {"contentType": "text/html", "url": "Binary/bn-html"}},
         {"attachment": {"contentType": "text/plain", "data": b64("Inline text, café ☕".encode())}}]},
    {"resourceType": "DocumentReference", "id": "dr-1001", "status": "current", "subject": {"reference": "Patient/pt-1000"},
     "date": "2025-01-02T00:00:00Z", "content": [{"attachment": {"contentType": "text/plain", "url": "Binary/bn-shared"}}]},
    {"resourceType": "DocumentReference", "id": "dr-1002", "status": "superseded", "subject": {"reference": "Patient/pt-1000"},
     "date": "2025-01-03T00:00:00Z", "content": [
         {"attachment": {"contentType": "text/plain", "url": "Binary/bn-shared"}},
         {"attachment": {"contentType": "text/plain", "url": "Binary/bn-fake-text"}},
         {"attachment": {"contentType": "text/plain", "url": "Binary/bn-spaces"}},
         {"attachment": {"contentType": "text/plain", "url": "#contained-1"}}]},
    {"resourceType": "DocumentReference", "id": "dr-1003", "status": "entered-in-error", "subject": {"reference": "Patient/pt-1000"},
     "date": "2025-01-04T00:00:00Z", "content": [{"attachment": {"contentType": "text/plain", "data": b64(b"Wrong patient.")}}]},
])

# 11. Values: other value types, units, extremes, wrong types.
write("11-values.jsonl", [
    patient("pt-1100", ["Val"], "Ues", "1990-09-09"),
    obs("ob-1100", "pt-1100", GLUCOSE, "2025-01-01T08:00:00Z", 6.1, "mmol/L"),       # other unit: kept as is
    obs("ob-1101", "pt-1100", GLUCOSE, "2025-01-02T08:00:00Z", 0, "mg/dL"),          # zero: implausible
    obs("ob-1102", "pt-1100", WEIGHT, "2025-01-03T08:00:00Z", -5, "kg"),             # negative: impossible
    obs("ob-1103", "pt-1100", WEIGHT, "2025-01-04T08:00:00Z", 1e9, "kg"),            # huge: implausible
    {**obs("ob-1104", "pt-1100", WEIGHT, "2025-01-05T08:00:00Z"), "valueQuantity": {"value": True, "unit": "kg"}},
    {**obs("ob-1105", "pt-1100", WEIGHT, "2025-01-06T08:00:00Z"), "valueQuantity": {"value": None, "unit": "kg"}},
    {**obs("ob-1106", "pt-1100", loinc("8302-2", "Body height"), "2025-01-07T08:00:00Z"), "valueInteger": 170},
    {**obs("ob-1107", "pt-1100", loinc("8310-5", "Body temperature"), "2025-01-08T08:00:00Z"), "valueString": "normal"},
    {**obs("ob-1108", "pt-1100", WEIGHT, "2025-01-09T08:00:00Z"), "dataAbsentReason": {"text": "Patient refused"}},
    obs("ob-1109", "pt-1100", A1C, "2025-01-10T08:00:00Z", 6.0, "%", status="preliminary"),
    obs("ob-1110", "pt-1100", A1C, "2025-01-11T08:00:00Z", 6.0, "%", status="cancelled"),
])

# 12. Many records, to check speed. 2,000 patients, 20,000 observations.
records = []
for i in range(2000):
    pid = "bulk-%04d" % i
    records.append(patient(pid, ["Bulk"], "Patient%04d" % i, "%d-01-01" % (1940 + i % 60)))
    for j in range(10):
        records.append(obs("%s-ob-%d" % (pid, j), pid, A1C, "2024-%02d-01T08:00:00Z" % (j + 1), 5 + (i + j) % 40 / 10, "%"))
write("12-bulk.jsonl", records)

print("wrote", len(os.listdir(OUT)), "files to", OUT)
