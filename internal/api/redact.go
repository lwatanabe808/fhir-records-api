package api

import (
	"encoding/json"
	"strings"

	"github.com/lwatanabe808/novellia-fhir-api/internal/loader"
)

// redactRaw masks SSNs in an original FHIR record before it leaves the API.
// raw_resources keeps the record exactly as received, so every endpoint that
// returns raw JSON (/fhir, /records, issues with includeRaw) passes it
// through here. Records without an SSN are returned unchanged, byte for byte.
func redactRaw(raw string) string {
	if !strings.Contains(raw, loader.SSNSystem) {
		return raw
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	maskSSNs(v)
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"record could not be redacted"}`
	}
	return string(b)
}

func maskSSNs(v any) {
	switch t := v.(type) {
	case map[string]any:
		if t["system"] == loader.SSNSystem {
			if _, ok := t["value"]; ok {
				t["value"] = "[redacted]"
			}
		}
		for _, child := range t {
			maskSSNs(child)
		}
	case []any:
		for _, child := range t {
			maskSSNs(child)
		}
	}
}
