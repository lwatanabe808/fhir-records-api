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
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// fhir.schema.json is the official FHIR R4 JSON schema from
// https://hl7.org/fhir/R4/fhir.schema.json.zip
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
	compiler *jsonschema.Compiler
	types    map[string]bool

	mu       sync.Mutex
	compiled map[string]*jsonschema.Schema
}

// NewSchema loads the embedded FHIR schema.
func NewSchema() (*Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return nil, fmt.Errorf("read FHIR schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft6)
	if err := c.AddResource(schemaURL, doc); err != nil {
		return nil, err
	}

	// The schema lists every official resource type in its discriminator mapping.
	var meta struct {
		Discriminator struct {
			Mapping map[string]string `json:"mapping"`
		} `json:"discriminator"`
	}
	if err := json.Unmarshal(schemaJSON, &meta); err != nil {
		return nil, err
	}
	types := map[string]bool{}
	for t := range meta.Discriminator.Mapping {
		types[t] = true
	}
	return &Schema{compiler: c, types: types, compiled: map[string]*jsonschema.Schema{}}, nil
}

// IsFHIRType reports whether the name is one of the official R4 resource types.
func (s *Schema) IsFHIRType(resourceType string) bool {
	return s.types[resourceType]
}

// Validate checks one record against the schema for its own type only.
// Checking against the whole schema would report errors for all ~146 types.
func (s *Schema) Validate(resourceType string, rawLine []byte) ([]Problem, error) {
	sch, err := s.schemaFor(resourceType)
	if err != nil {
		return nil, err
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
	var out []Problem
	collectLeaves(ve, &out)
	return out, nil
}

func (s *Schema) schemaFor(resourceType string) (*jsonschema.Schema, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sch, ok := s.compiled[resourceType]; ok {
		return sch, nil
	}
	sch, err := s.compiler.Compile(schemaURL + "#/definitions/" + resourceType)
	if err != nil {
		return nil, err
	}
	s.compiled[resourceType] = sch
	return sch, nil
}

// collectLeaves walks the error tree and keeps only the most specific errors.
func collectLeaves(ve *jsonschema.ValidationError, out *[]Problem) {
	if len(ve.Causes) > 0 {
		for _, c := range ve.Causes {
			collectLeaves(c, out)
		}
		return
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
	*out = append(*out, p)
}
