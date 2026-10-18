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
show "Summary for the app home screen" "/patients/noah-wyle/summary"
show "Conditions" "/patients/noah-wyle/conditions"
show "Medications (note med-nw-003 was linked despite Patient/Noah-Wyle)" "/patients/noah-wyle/medications"
show "A1c results (code=a1c is a shortcut for LOINC 4548-4); the conflicting duplicate shows a warning" "/patients/noah-wyle/observations?code=a1c"
show "Everything, including hidden records and why they are hidden" "/patients/noah-wyle/observations?code=a1c&includeHidden=true"
show "Blood pressure split into systolic and diastolic" "/patients/noah-wyle/observations?code=blood-pressure"
show "Tracy's oxygen: the amended 96% replaces the original 94%" "/patients/tracy-ifeachor/observations?code=oxygen&includeHidden=true"
show "One record by id, even a hidden one (the 7.9 duplicate)" "/patients/noah-wyle/observations/obs-nw-dup-001"
show "Procedures" "/patients/noah-wyle/procedures"
show "Notes with decoded text" "/patients/noah-wyle/notes"
show "Search this patient's notes" "/patients/noah-wyle/notes?q=shift"
show "Timeline, newest first" "/patients/noah-wyle/timeline"
show "Raw records of any type, including non-standard ones" "/patients/noah-wyle/records"

# Research partners (B2B)
show "Cohort: diabetes with latest A1c of 7 or more (de-identified)" "/research/cohort?condition=diabetes&lab=a1c&labMin=7"
show "Cohort: women aged 30 to 45" "/research/cohort?gender=female&minAge=30&maxAge=45"
show "Cohort with exclusion: diabetes, but not high cholesterol" "/research/cohort?condition=diabetes&excludeCondition=hypercholesterolemia"
show "Cohort by procedure" "/research/cohort?procedure=colonoscopy"
show "Dataset profile: history, completeness, coding, quality, population" "/research/dataset-profile"
show "How many patients have each condition" "/research/conditions"
show "Latest A1c across patients: median and range" "/research/labs/a1c"

# Data quality
show "Data quality summary" "/data-quality/summary"
show "All issues" "/data-quality/issues"
show "Only warnings about patient links" "/data-quality/issues?type=UNKNOWN_PATIENT&includeRaw=true"

# Original FHIR record
show "Original record, untouched" "/fhir/Observation/obs-nw-dup-001"
show "Every version of a record" "/fhir/Observation/obs-nw-dup-001/history"

# Errors all share one shape
show "Bad parameter (400)" "/patients?limit=abc"
show "Unknown patient (404)" "/patients/nobody"
