#!/usr/bin/env sh
# Calls each endpoint with cURL. Start the API first (make run or make docker).
# Usage: ./scripts/demo.sh [base-url]
BASE="${1:-http://localhost:8080}"

show() {
  echo
  echo "### $1"
  echo "curl -s \"$BASE$2\""
  curl -s "$BASE$2"
  echo
}

# Health and load summary
show "Health and load summary" "/health"

# Patient app
show "List patients, staff only (search by name)" "/internal/patients?name=wyle"
show "One patient (ids work in any capitalization)" "/patients/NOAH-WYLE"
show "Conditions" "/patients/noah-wyle/conditions"
show "Medications (note med-nw-003 was linked despite Patient/Noah-Wyle)" "/patients/noah-wyle/medications"
show "A1c results (LOINC 4548-4); only one of the two conflicting duplicates is shown" "/patients/noah-wyle/observations?code=4548-4"
show "Blood pressure (LOINC 85354-9), split into systolic and diastolic" "/patients/noah-wyle/observations?code=85354-9"
show "Tracy's oxygen: only the amended 96% is shown (it replaced the original 94%)" "/patients/tracy-ifeachor/observations?code=59408-5"
show "Procedures" "/patients/noah-wyle/procedures"
show "Notes with decoded text" "/patients/noah-wyle/notes"

# Research partners (B2B)
show "Cohort: diabetes with latest A1c (LOINC 4548-4) of 7 or more (de-identified)" "/research/cohort?condition=diabetes&lab=4548-4&labMin=7"
show "Cohort by keyword: \"diabetes\" is broad and also matches prediabetes" "/research/cohort?condition=diabetes"
show "Cohort by disease code: type 2 diabetes only (SNOMED 44054006)" "/research/cohort?condition=44054006"
show "Cohort by codes: type 2 diabetes on Metformin 500 MG (RxNorm 860975)" "/research/cohort?condition=44054006&medication=860975"
show "Cohort excluding by code: type 2 diabetes without hypercholesterolemia (SNOMED 13644009)" "/research/cohort?condition=44054006&excludeCondition=13644009"
show "Cohort: women aged 30 to 45" "/research/cohort?gender=female&minAge=30&maxAge=45"
show "Cohort with exclusion: diabetes, but not high cholesterol" "/research/cohort?condition=diabetes&excludeCondition=hypercholesterolemia"
show "Cohort by procedure" "/research/cohort?procedure=colonoscopy"
show "Dataset profile: patients, history depth, population" "/research/dataset-profile"

# Internal, staff only: data quality
show "Data quality summary" "/internal/data-quality/summary"
show "All issues" "/internal/data-quality/issues"
show "Only problems with patient links" "/internal/data-quality/issues?type=UNKNOWN_PATIENT&includeRaw=true"

# Original FHIR record
show "Original record, untouched" "/internal/fhir/Observation/obs-nw-dup-001"

# Errors all share one shape
show "Bad parameter (400)" "/internal/patients?limit=abc"
show "Unknown patient (404)" "/patients/nobody"
