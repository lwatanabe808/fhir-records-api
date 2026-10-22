# Status values

The statuses each record type can have, from FHIR R4's official value sets, and how the API uses them.

| Field | Values | Meaning |
|---|---|---|
| Condition `status` ([clinical status](https://hl7.org/fhir/R4/valueset-condition-clinical.html)) | `active`, `recurrence`, `relapse`, `inactive`, `remission`, `resolved` | Where the condition stands now. `?status=` filters on this |
| Condition `verification` ([verification status](https://hl7.org/fhir/R4/valueset-condition-ver-status.html)) | `confirmed`, `provisional`, `differential`, `unconfirmed`, `refuted`, `entered-in-error` | How sure the doctor is. Research leaves out `refuted` and `entered-in-error` |
| Medication `status` ([MedicationRequest status](https://hl7.org/fhir/R4/valueset-medicationrequest-status.html)) | `active`, `on-hold`, `completed`, `stopped`, `cancelled`, `draft`, `unknown`, `entered-in-error` | Where the prescription stands. `active` means the patient should be taking it now |
| Observation `status` ([observation status](https://hl7.org/fhir/R4/valueset-observation-status.html)) | `registered`, `preliminary`, `final`, `amended`, `corrected`, `cancelled`, `entered-in-error`, `unknown` | Whether the result is final. `amended` and `corrected` replace earlier readings |
| Procedure `status` ([event status](https://hl7.org/fhir/R4/valueset-event-status.html)) | `preparation`, `in-progress`, `not-done`, `on-hold`, `stopped`, `completed`, `entered-in-error`, `unknown` | Whether it was done |

Records marked `entered-in-error`, and cancelled observations, are never returned.

Back to the [README](../README.md).
