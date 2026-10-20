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
show "List patients (search by name, filter by gender or birth date)" "/patients?name=wyle"
show "One patient (ids work in any capitalization)" "/patients/NOAH-WYLE"
show "Conditions" "/patients/noah-wyle/conditions"
show "Medications (note med-nw-003 was linked despite Patient/Noah-Wyle)" "/patients/noah-wyle/medications"
show "A1c results (LOINC 4548-4); the conflicting duplicate shows a warning" "/patients/noah-wyle/observations?code=4548-4"
show "Blood pressure (LOINC 85354-9), split into systolic and diastolic" "/patients/noah-wyle/observations?code=85354-9"
show "Tracy's oxygen: only the amended 96% is shown (it replaced the original 94%)" "/patients/tracy-ifeachor/observations?code=59408-5"
show "Procedures" "/patients/noah-wyle/procedures"
show "Notes with decoded text" "/patients/noah-wyle/notes"
show "Timeline, newest first" "/patients/noah-wyle/timeline"

# Research partners (B2B)
show "Cohort: diabetes with latest A1c (LOINC 4548-4) of 7 or more (de-identified)" "/research/cohort?condition=diabetes&lab=4548-4&labMin=7"
show "Cohort: women aged 30 to 45" "/research/cohort?gender=female&minAge=30&maxAge=45"
show "Cohort with exclusion: diabetes, but not high cholesterol" "/research/cohort?condition=diabetes&excludeCondition=hypercholesterolemia"
show "Cohort by procedure" "/research/cohort?procedure=colonoscopy"
show "Dataset profile: patients, history depth, population" "/research/dataset-profile"

# Data quality
show "Data quality summary" "/data-quality/summary"
show "All issues" "/data-quality/issues"
show "Only warnings about patient links" "/data-quality/issues?type=UNKNOWN_PATIENT&includeRaw=true"

# Original FHIR record
show "Original record, untouched" "/fhir/Observation/obs-nw-dup-001"

# Errors all share one shape
show "Bad parameter (400)" "/patients?limit=abc"
show "Unknown patient (404)" "/patients/nobody"
