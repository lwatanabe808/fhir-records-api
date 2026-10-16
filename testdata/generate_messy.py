"""Builds testdata/messy.jsonl: one or more planted problems for every case in
the README's validation table. Run from the repo root: python3 testdata/generate_messy.py
"""
import base64
import itertools
import json

lines = []
day = itertools.count(1)


def add(o):
    lines.append(o if isinstance(o, str) else json.dumps(o))


def loinc(code, display):
    return {"coding": [{"system": "http://loinc.org", "code": code, "display": display}]}


A1C = loinc("4548-4", "Hemoglobin A1c")
O2 = loinc("59408-5", "Oxygen saturation")


def obs(i, subj="Patient/p1", code=A1C, t=None, val=7.0, unit="%", status="final", **extra):
    # Each observation gets its own time unless one is given, so only the
    # records meant to be duplicates share a patient, lab test code and time.
    t = t or "2024-%02d-%02dT08:00:00Z" % (next(day) % 12 + 1, 10)
    o = {"resourceType": "Observation", "id": i, "status": status, "code": code, "effectiveDateTime": t}
    if subj is not None:
        o["subject"] = {"reference": subj} if isinstance(subj, str) and subj.startswith("Patient/") else subj
    if val is not None:
        o["valueQuantity"] = {"value": val, "unit": unit} if unit else {"value": val}
    o.update(extra)
    return o


def b64(b):
    return base64.b64encode(b).decode()


# Patients (same name and birth date: possible duplicate, never merged)
add({"resourceType": "Patient", "id": "p1", "name": [{"family": "Lee", "given": ["Ann"]}], "gender": "female", "birthDate": "1980-01-01"})
add({"resourceType": "Patient", "id": "p2", "name": [{"family": "Lee", "given": ["Ann"]}], "gender": "female", "birthDate": "1980-01-01"})

# Parsing
add('{"resourceType":"Observ')                       # invalid JSON
add("")                                              # blank line
add("[1,2,3]")                                       # valid JSON, not a record
add('{"id":"x"}')                                    # no resourceType
add({"resourceType": "ClinicalNote", "id": "cn1", "subject": {"reference": "Patient/p1"}})
add({"resourceType": "Immunization", "id": "imm1", "status": "completed", "vaccineCode": {"text": "Flu"},
     "patient": {"reference": "Patient/p1"}, "occurrenceDateTime": "2024-10-01"})
o = obs("ignored"); del o["id"]; add(o)              # no id

# Schema problems
add(obs("o-status", status="done"))
add(obs("o-date", t="2025-13-45T08:00:00Z"))
add(obs("o-valtype", val="7.8"))
add(obs("o-subjstr", subj="p1"))                     # subject is a plain string

# Own checks
add(obs("o-noprefix", subj={"reference": "p1"}))
o = obs("o-nosubject"); del o["subject"]; add(o)
add(obs("o-nameonly", subj={"display": "Ann Lee"}))
add(obs("o-nocode", code={"text": "Grip strength"}))
add(obs("o-nosystem", code={"coding": [{"code": "4548-4"}]}))
add(obs("o-novalue", val=None))
add(obs("o-multi", valueString="7.1"))
add(obs("o-nounit", code=loinc("29463-7", "Body weight"), val=88, unit=None))
add(obs("o-o2", code=O2, val=104))
add(obs("o-bp", code=loinc("85354-9", "Blood pressure panel"), val=None, component=[
    {"code": loinc("8480-6", "Systolic"), "valueQuantity": {"value": 80, "unit": "mmHg"}},
    {"code": loinc("8462-4", "Diastolic"), "valueQuantity": {"value": 120, "unit": "mmHg"}}]))
add(obs("o-future", t="2099-01-01T00:00:00Z"))
add(obs("o-implausible", val=78))
add(obs("o-prebirth", t="1970-05-05T00:00:00Z"))
add({"resourceType": "MedicationRequest", "id": "m-empty", "status": "active", "intent": "order",
     "medicationCodeableConcept": {"coding": [{"system": "http://www.nlm.nih.gov/research/umls/rxnorm", "code": "860975", "display": "Metformin 500 MG"}]},
     "subject": {"reference": "Patient/p1"}, "authoredOn": "2020-01-01", "dosageInstruction": []})

# Links (pass 2)
add(obs("o-case", subj="Patient/P1"))
add(obs("o-unknown", subj="Patient/nobody"))

# Duplicates and corrections (pass 2)
add(obs("o-dupid", t="2025-02-01T08:00:00Z"))
add(obs("o-dupid", t="2025-02-01T08:00:00Z"))       # identical copy
add(obs("o-changed", t="2025-02-02T08:00:00Z", val=6.0))
add(obs("o-changed", t="2025-02-02T08:00:00Z", val=6.5))  # changed copy, later wins
add(obs("o-g1", t="2025-03-01T08:00:00Z", val=7.8))
add(obs("o-g2", t="2025-03-01T08:00:00Z", val=7.9))  # same test and time, different value
add(obs("o-s1", t="2025-03-02T08:00:00Z", val=7.0))
add(obs("o-s2", t="2025-03-02T08:00:00Z", val=7.0))  # same test, time and value
add(obs("o-orig", code=O2, t="2025-04-01T08:00:00Z", val=94))
add(obs("o-amend", code=O2, t="2025-04-01T08:00:00Z", val=96, status="amended"))
add(obs("o-eie", t="2025-05-01T08:00:00Z", val=9.9, status="entered-in-error"))

# Conditions (p2's diagnosis was ruled out)
diabetes = {"coding": [{"system": "http://snomed.info/sct", "code": "44054006", "display": "Type 2 Diabetes Mellitus"}]}
add({"resourceType": "Condition", "id": "c-diab", "clinicalStatus": {"coding": [{"code": "active"}]},
     "verificationStatus": {"coding": [{"code": "confirmed"}]}, "code": diabetes,
     "subject": {"reference": "Patient/p1"}, "onsetDateTime": "2018-07-22"})
add({"resourceType": "Condition", "id": "c-refuted", "clinicalStatus": {"coding": [{"code": "active"}]},
     "verificationStatus": {"coding": [{"code": "refuted"}]}, "code": diabetes,
     "subject": {"reference": "Patient/p2"}, "onsetDateTime": "2019-01-01"})

# Binaries
png = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
add({"resourceType": "Binary", "id": "b-bad", "contentType": "text/plain", "data": "%%%not-base64%%%"})
add({"resourceType": "Binary", "id": "b-nocontent", "data": b64(b"hello")})
add({"resourceType": "Binary", "id": "b-badtype", "contentType": "pdf", "data": b64(b"hello")})
add({"resourceType": "Binary", "id": "b-png", "contentType": "text/plain", "data": b64(png)})
add({"resourceType": "Binary", "id": "b-orphan", "contentType": "text/plain", "data": b64(b"Nobody links here.")})
add({"resourceType": "Binary", "id": "b-text", "contentType": "text/plain", "data": b64(b"Patient is stable.")})
add({"resourceType": "Binary", "id": "b-big", "contentType": "text/plain", "data": b64(b"x" * 200)})


def att(**k):
    return {"attachment": k}


add({"resourceType": "DocumentReference", "id": "d1", "status": "current",
     "type": {"coding": [{"system": "http://loinc.org", "code": "11488-4", "display": "Consult note"}]},
     "subject": {"reference": "Patient/p1"}, "date": "2025-01-05T08:00:00Z", "content": [
         att(contentType="text/plain", data=b64(b"Inline note text.")),        # inline
         att(contentType="application/pdf", url="Binary/b-text"),              # type mismatch
         att(contentType="text/plain", url="Binary/b-missing"),                # broken link
         att(contentType="application/pdf", url="https://files.example.com/report.pdf"),
         att(contentType="text/plain", url="abc"),                             # invalid url
         att(contentType="text/plain"),                                        # no url, no data
         att(url="Binary/b-bad"), att(url="Binary/b-nocontent"), att(url="Binary/b-badtype"),
         att(url="Binary/b-png"), att(url="Binary/b-big")]})

# Too large (the tests use a 2000 byte limit)
add(obs("o-huge", note=[{"text": "x" * 3000}]))

with open("testdata/messy.jsonl", "w") as f:
    f.write("\n".join(lines) + "\n")
print(len(lines), "lines")
