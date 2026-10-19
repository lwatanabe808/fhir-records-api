// Package validate checks records against the official FHIR R4 JSON schema
// and against simple "impossible value" rules.
package validate

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// fhir.schema.json is the official FHIR R4 JSON schema from
// https://hl7.org/fhir/R4/fhir.schema.json.zip. go:embed copies it into
// schemaJSON when the program is built.
//
//go:embed fhir.schema.json
var schemaJSON []byte

const schemaURL = "file:///fhir.schema.json"

// Problem is one schema error, already reduced to something readable.
type Problem struct {
	Path    string // e.g. "/status" or "/valueQuantity/value"
	Keyword string // enum, type, pattern, required, additionalProperties...
	Message string
}

// Schema validates records against the FHIR R4 schema, one resource type at a time.
type Schema struct {
	types    map[string]string             // official type name → its rules, e.g. "Patient" → "#/definitions/Patient"
	checkers map[string]*jsonschema.Schema // one ready-made checker per type we check
}

// NewSchema loads the embedded FHIR schema and builds a checker for each of
// the given record types. Building is slow, so it happens once, at startup.
func NewSchema(checkTypes []string) (*Schema, error) {
	schemaJSONBytesReader := bytes.NewReader(schemaJSON)
	doc, err := jsonschema.UnmarshalJSON(schemaJSONBytesReader)
	if err != nil {
		return nil, fmt.Errorf("read FHIR schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	// The schema doc says it's written in draft 6
	c.DefaultDraft(jsonschema.Draft6)
	// add the unmarshaled schema to the compiler and name it file:///fhir.schema.json
	if err := c.AddResource(schemaURL, doc); err != nil {
		return nil, err
	}

	// In the json schema, discriminator.mapping contains the valid record types
	var meta struct {
		Discriminator struct {
			Mapping map[string]string `json:"mapping"`
		} `json:"discriminator"`
	}
	if err := json.Unmarshal(schemaJSON, &meta); err != nil {
		return nil, err
	}

	// Build each checker: Compile gathers that type's rules, following every
	// link to shared rules (Reference, CodeableConcept...), into one checker.
	checkers := map[string]*jsonschema.Schema{}
	for _, t := range checkTypes {
		sch, err := c.Compile(schemaURL + "#/definitions/" + t)
		if err != nil {
			return nil, fmt.Errorf("build %s checker: %w", t, err)
		}
		checkers[t] = sch
	}
	return &Schema{types: meta.Discriminator.Mapping, checkers: checkers}, nil
}

// IsFHIRType reports whether the name is one of the official R4 resource types.
func (s *Schema) IsFHIRType(resourceType string) bool {
	_, ok := s.types[resourceType]
	return ok
}

// Validate checks one record against the schema for its own type only.
// Checking against the whole schema would report errors for all ~146 types.
func (s *Schema) Validate(resourceType string, rawLine []byte) ([]Problem, error) {
	sch, ok := s.checkers[resourceType]
	if !ok {
		return nil, fmt.Errorf("no checker for %s", resourceType)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawLine))
	if err != nil {
		return nil, err
	}
	err = sch.Validate(inst)
	if err == nil {
		return nil, nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil, err
	}
	return collectLeaves(ve), nil
}

// collectLeaves walks the error tree and keeps only the most specific errors
// (the leaves): "status: value done is not allowed", not "the record is invalid".
func collectLeaves(ve *jsonschema.ValidationError) []Problem {
	if len(ve.Causes) > 0 {
		var out []Problem
		for _, c := range ve.Causes {
			out = append(out, collectLeaves(c)...)
		}
		return out
	}
	p := Problem{Path: "/" + strings.Join(ve.InstanceLocation, "/")}
	switch k := ve.ErrorKind.(type) {
	case *kind.Enum:
		p.Keyword, p.Message = "enum", fmt.Sprintf("value %v is not allowed", k.Got)
	case *kind.Type:
		p.Keyword, p.Message = "type", fmt.Sprintf("expected %s, got %s", strings.Join(k.Want, " or "), k.Got)
	case *kind.Pattern:
		p.Keyword, p.Message = "pattern", fmt.Sprintf("%q is not in the required format", k.Got)
	case *kind.Required:
		p.Keyword, p.Message = "required", "missing "+strings.Join(k.Missing, ", ")
	case *kind.AdditionalProperties:
		p.Keyword, p.Message = "additionalProperties", "unknown field "+strings.Join(k.Properties, ", ")
	default:
		p.Keyword, p.Message = "other", ve.Error()
	}
	return []Problem{p}
}
