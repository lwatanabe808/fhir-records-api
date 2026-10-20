package validate

import "fmt"

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
