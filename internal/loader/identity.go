package loader

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/lwatanabe808/novellia-fhir-api/internal/fhir"
)

// SSNSystem is the official FHIR identifier system for US Social Security numbers.
const SSNSystem = "http://hl7.org/fhir/sid/us-ssn"

// nameKey turns a name into a form for matching: lowercase, accents removed,
// punctuation turned into spaces, spaces collapsed. "GARCÍA, María" and
// "maria garcia" give the same key. Middle names are kept, so "Jalen Thomas
// Brooks" and "Jalen Brooks" do not match: a missed duplicate is safer than
// a false one.
func nameKey(name string) string {
	stripAccents := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	plain, _, err := transform.String(stripAccents, name)
	if err != nil {
		plain = name
	}
	var b strings.Builder
	for _, r := range strings.ToLower(plain) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// patientSSN finds the patient's SSN identifier. It returns the digits and
// a reason when the number can't be used for matching.
func patientSSN(identifiers []fhir.Identifier) (digits string, found bool, problem string) {
	for _, ident := range identifiers {
		if ident.System != SSNSystem {
			continue
		}
		value := ident.Value
		var b strings.Builder
		for _, c := range value {
			if c >= '0' && c <= '9' {
				b.WriteRune(c)
			}
		}
		digits = b.String()
		switch {
		case len(digits) == 4:
			return "", true, "only the last 4 digits, too weak to match on"
		case len(digits) != 9:
			return "", true, "not 9 digits"
		case !validSSN(digits):
			return "", true, "not a possible SSN (a placeholder or invalid number)"
		}
		return digits, true, ""
	}
	return "", false, ""
}

// validSSN rejects numbers the Social Security Administration never issues,
// and well-known placeholder numbers that clinics sometimes enter.
func validSSN(d string) bool {
	area, group, serial := d[:3], d[3:5], d[5:]
	if area == "000" || area == "666" || area[0] == '9' || group == "00" || serial == "0000" {
		return false
	}
	switch d {
	case "123456789", "078051120", "219099999":
		return false
	}
	return true
}

// hashSSN keeps only a keyed hash of the SSN. The database can match people
// on it without ever holding the number, and without the key nobody can
// rebuild it by hashing every possible SSN.
func hashSSN(secret []byte, digits string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(digits))
	return hex.EncodeToString(mac.Sum(nil))
}
