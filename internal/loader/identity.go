package loader

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// SSNSystem is the official FHIR identifier system for US Social Security numbers.
// SSNs are never used or stored by the loader; the API masks them in raw output.
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
