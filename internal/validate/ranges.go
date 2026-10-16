package validate

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

// Range is a wide "plausible" range for one lab test, in one unit.
type Range struct {
	Name string  `json:"name"`
	Unit string  `json:"unit"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
}

// The default ranges are placeholder values: deliberately wide limits meant to
// catch typos (a slipped decimal, a wrong unit), not medical judgments.
// A clinician should review them. Override with PLAUSIBLE_RANGES_FILE.
//
//go:embed plausible-ranges.json
var defaultRanges []byte

// Ranges maps a LOINC lab test code to its plausible range.
type Ranges map[string]Range

// LoadRanges reads the ranges file, or the built-in defaults if path is "".
func LoadRanges(path string) (Ranges, error) {
	data := defaultRanges
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read ranges file: %w", err)
		}
		data = b
	}
	var r Ranges
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse ranges file: %w", err)
	}
	return r, nil
}

// CheckImpossible returns a reason if a value cannot be real, or "".
// These rules need no medical judgment.
func CheckImpossible(value float64, unit string) string {
	if value < 0 {
		return fmt.Sprintf("negative value %v", value)
	}
	if unit == "%" && value > 100 {
		return fmt.Sprintf("percentage %v is over 100", value)
	}
	return ""
}

// CheckImplausible returns a reason if a value is outside the wide range for
// its lab test, or "". Unknown tests and other units are not checked.
func (r Ranges) CheckImplausible(code string, value float64, unit string) string {
	rg, ok := r[code]
	if !ok || unit != rg.Unit {
		return ""
	}
	if value < rg.Min || value > rg.Max {
		return fmt.Sprintf("%s %v %s is outside the plausible range %v to %v", rg.Name, value, unit, rg.Min, rg.Max)
	}
	return ""
}
