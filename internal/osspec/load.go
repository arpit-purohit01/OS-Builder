package osspec

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"foundry.dev/foundry/internal/jcs"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// MaxSpecBytes bounds spec documents accepted from users.
const MaxSpecBytes = 1 << 20

//go:embed schema.v1alpha1.json
var schemaJSON []byte

// SchemaJSON returns the embedded JSON Schema (served to the web UI).
func SchemaJSON() []byte { return append([]byte(nil), schemaJSON...) }

// FieldError is one validation problem at a JSON pointer.
type FieldError struct {
	Pointer string `json:"pointer"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ValidationError aggregates problems from one validation layer.
type ValidationError struct {
	Layer  string       `json:"layer"`
	Errors []FieldError `json:"errors"`
}

func (e *ValidationError) Error() string {
	var parts []string
	for _, f := range e.Errors {
		parts = append(parts, fmt.Sprintf("%s %s: %s", f.Code, f.Pointer, f.Message))
	}
	return fmt.Sprintf("spec invalid (%s): %s", e.Layer, strings.Join(parts, "; "))
}

// Loaded is a fully validated (layers 1–2), defaulted, canonical spec.
type Loaded struct {
	Spec      *Spec
	Canonical []byte // RFC 8785 JSON of the defaulted spec
	Digest    string // "sha256:<hex>" of Canonical
	Warnings  []FieldError
	Migrated  bool
}

var (
	compiledOnce sync.Once
	compiled     *jsonschema.Schema
	compileErr   error
)

func schema() (*jsonschema.Schema, error) {
	compiledOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
		if err != nil {
			compileErr = err
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		const url = "https://schemas.foundry.dev/osspec/v1alpha1.json"
		if err := c.AddResource(url, doc); err != nil {
			compileErr = err
			return
		}
		compiled, compileErr = c.Compile(url)
	})
	return compiled, compileErr
}

// ParseDocument parses YAML or JSON into a JSON-compatible generic document.
func ParseDocument(data []byte) (map[string]any, error) {
	if len(data) > MaxSpecBytes {
		return nil, &ValidationError{Layer: "parse", Errors: []FieldError{{Pointer: "", Code: "PARSE-001", Message: "spec larger than 1 MiB"}}}
	}
	trimmed := bytes.TrimSpace(data)
	var v any
	if len(trimmed) > 0 && trimmed[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, &ValidationError{Layer: "parse", Errors: []FieldError{{Code: "PARSE-002", Message: "invalid JSON: " + err.Error()}}}
		}
	} else {
		var node yaml.Node
		if err := yaml.Unmarshal(data, &node); err != nil {
			return nil, &ValidationError{Layer: "parse", Errors: []FieldError{{Code: "PARSE-003", Message: "invalid YAML: " + err.Error()}}}
		}
		if err := node.Decode(&v); err != nil {
			return nil, &ValidationError{Layer: "parse", Errors: []FieldError{{Code: "PARSE-003", Message: "invalid YAML: " + err.Error()}}}
		}
	}
	norm, err := normalize(v)
	if err != nil {
		return nil, &ValidationError{Layer: "parse", Errors: []FieldError{{Code: "PARSE-004", Message: err.Error()}}}
	}
	m, ok := norm.(map[string]any)
	if !ok {
		return nil, &ValidationError{Layer: "parse", Errors: []FieldError{{Code: "PARSE-005", Message: "spec must be an object"}}}
	}
	return m, nil
}

// normalize converts YAML-decoded values to JSON-compatible ones.
func normalize(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("non-string key %v", k)
			}
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			out[ks] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case time.Time:
		return t.UTC().Format(time.RFC3339), nil
	case int:
		return json.Number(fmt.Sprint(t)), nil
	case int64:
		return json.Number(fmt.Sprint(t)), nil
	case uint64:
		return json.Number(fmt.Sprint(t)), nil
	case float64:
		return t, nil // schema rejects floats where integers/strings are expected
	default:
		return v, nil
	}
}

// Load runs parse → migrate → schema → decode → defaults → semantic → canonicalize.
func Load(data []byte) (*Loaded, error) {
	doc, err := ParseDocument(data)
	if err != nil {
		return nil, err
	}
	return LoadDocument(doc)
}

// LoadDocument is Load for an already parsed document.
func LoadDocument(doc map[string]any) (*Loaded, error) {
	migrated, warnings, err := Migrate(doc)
	if err != nil {
		return nil, err
	}
	sch, err := schema()
	if err != nil {
		return nil, fmt.Errorf("osspec: schema: %w", err)
	}
	raw, err := json.Marshal(migrated.doc)
	if err != nil {
		return nil, err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if err := sch.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return nil, &ValidationError{Layer: "schema", Errors: schemaErrors(ve)}
		}
		return nil, err
	}
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, &ValidationError{Layer: "schema", Errors: []FieldError{{Code: "SCH-002", Message: err.Error()}}}
	}
	Default(&s)
	semErrs, semWarn := Semantic(&s)
	warnings = append(warnings, semWarn...)
	if len(semErrs) > 0 {
		return nil, &ValidationError{Layer: "semantic", Errors: semErrs}
	}
	canon, err := jcs.Marshal(&s)
	if err != nil {
		return nil, err
	}
	return &Loaded{Spec: &s, Canonical: canon, Digest: Digest(canon), Warnings: warnings, Migrated: migrated.changed}, nil
}

// Digest returns "sha256:<hex>" of canonical bytes.
func Digest(canonical []byte) string {
	h := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(h[:])
}

func schemaErrors(ve *jsonschema.ValidationError) []FieldError {
	out := ve.BasicOutput()
	seen := map[string]bool{}
	var errs []FieldError
	for _, u := range out.Errors {
		if u.Error == nil {
			continue
		}
		msgB, _ := json.Marshal(u.Error)
		var msg string
		_ = json.Unmarshal(msgB, &msg)
		if msg == "" {
			msg = string(msgB)
		}
		// Skip generic container errors when leaf errors exist.
		if strings.HasPrefix(msg, "validation failed") || strings.Contains(msg, "doesn't validate with") {
			continue
		}
		key := u.InstanceLocation + "|" + msg
		if seen[key] {
			continue
		}
		seen[key] = true
		errs = append(errs, FieldError{Pointer: u.InstanceLocation, Code: "SCH-001", Message: msg})
	}
	if len(errs) == 0 {
		errs = append(errs, FieldError{Code: "SCH-001", Message: ve.Error()})
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Pointer < errs[j].Pointer })
	return errs
}
