package osspec

import "fmt"

type migrated struct {
	doc     map[string]any
	changed bool
}

// migration converts a document of one apiVersion to the next.
type migration struct {
	from, to string
	fn       func(doc map[string]any) ([]FieldError, error)
}

// migrations is the ordered chain. Each step must be pure and tested with
// fixtures (old → new). v1alpha0 was the Phase 0 draft format.
var migrations = []migration{
	{
		from: "foundry.dev/v1alpha0",
		to:   "foundry.dev/v1alpha1",
		fn: func(doc map[string]any) ([]FieldError, error) {
			var warns []FieldError
			// v1alpha0 called the base "distro" and used "debian"/"ubuntu" without release.
			if f, ok := doc["foundation"].(map[string]any); ok {
				if d, ok := f["distro"].(string); ok {
					delete(f, "distro")
					switch d {
					case "debian":
						f["base"] = "debian-13"
					case "ubuntu":
						f["base"] = "ubuntu-24.04"
					default:
						f["base"] = d
					}
					warns = append(warns, FieldError{Pointer: "/foundation/distro", Code: "MIG-001", Message: "renamed to /foundation/base"})
				}
			}
			// v1alpha0 had a top-level "rootAccess" string.
			if ra, ok := doc["rootAccess"].(string); ok {
				delete(doc, "rootAccess")
				acc, _ := doc["access"].(map[string]any)
				if acc == nil {
					acc = map[string]any{}
					doc["access"] = acc
				}
				acc["rootModel"] = map[string]string{"none": "locked", "sudo": "admin-password", "full": "developer"}[ra]
				if acc["rootModel"] == "" {
					return nil, fmt.Errorf("unknown rootAccess %q", ra)
				}
				warns = append(warns, FieldError{Pointer: "/rootAccess", Code: "MIG-002", Message: "moved to /access/rootModel"})
			}
			return warns, nil
		},
	},
}

// Migrate upgrades doc in place to the current APIVersion.
func Migrate(doc map[string]any) (migrated, []FieldError, error) {
	v, _ := doc["apiVersion"].(string)
	var warns []FieldError
	changed := false
	for v != APIVersion {
		found := false
		for _, m := range migrations {
			if m.from == v {
				w, err := m.fn(doc)
				if err != nil {
					return migrated{}, nil, &ValidationError{Layer: "migrate", Errors: []FieldError{{Pointer: "/apiVersion", Code: "MIG-000", Message: err.Error()}}}
				}
				warns = append(warns, w...)
				doc["apiVersion"] = m.to
				v = m.to
				changed, found = true, true
				break
			}
		}
		if !found {
			return migrated{}, nil, &ValidationError{Layer: "migrate", Errors: []FieldError{{Pointer: "/apiVersion", Code: "MIG-404", Message: fmt.Sprintf("unsupported apiVersion %q", v)}}}
		}
	}
	return migrated{doc: doc, changed: changed}, warns, nil
}
