package catalog

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"foundry.dev/foundry/internal/osspec"
)

// TestWebSelectionsFixture writes raw specs with their Go Selections; the
// TypeScript port (web/src/solver/selections.ts) must agree exactly.
func TestWebSelectionsFixture(t *testing.T) {
	files, _ := filepath.Glob("../../test/golden/specs/*.yaml")
	ex, _ := filepath.Glob("../../docs/examples/*.yaml")
	files = append(files, ex...)
	var docs []map[string]any
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		d, err := osspec.ParseDocument(b)
		if err != nil {
			t.Fatal(f, err)
		}
		docs = append(docs, d)
	}
	r := rand.New(rand.NewPCG(7, 11))
	pick := func(xs ...string) string { return xs[r.IntN(len(xs))] }
	set := func(d map[string]any, path []string, v any) {
		m := d
		for _, k := range path[:len(path)-1] {
			n, ok := m[k].(map[string]any)
			if !ok {
				n = map[string]any{}
				m[k] = n
			}
			m = n
		}
		m[path[len(path)-1]] = v
	}
	base := len(docs)
	for i := 0; i < 300; i++ {
		var d map[string]any
		b, _ := json.Marshal(docs[r.IntN(base)])
		json.Unmarshal(b, &d)
		for j := 0; j < 1+r.IntN(5); j++ {
			switch r.IntN(12) {
			case 0:
				set(d, []string{"foundation", "profile"}, pick("desktop-kde", "desktop-gnome", "kiosk-cage", "thin-client", "server"))
			case 1:
				set(d, []string{"security", "diskEncryption"}, pick("none", "tpm2", "tpm2+pin", "passphrase"))
			case 2:
				set(d, []string{"access", "rootModel"}, pick("locked", "admin-password", "developer"))
			case 3:
				delete(d, "access")
			case 4:
				set(d, []string{"ecosystem", "handoff", "enabled"}, r.IntN(2) == 0)
			case 5:
				set(d, []string{"look", "layout"}, map[string]any{"preset": pick("dock-left", "minimal", "none")})
			case 6:
				delete(d, "look")
			case 7:
				set(d, []string{"hardware", "targets"}, []any{})
			case 8:
				set(d, []string{"apps", "preinstalled"}, []any{"flatpak:org.gimp.GIMP", "catalog:printing.cups"})
			case 9:
				set(d, []string{"security", "firewall", "allow"}, []any{pick("ssh", "rdp", "mdns")})
			case 10:
				set(d, []string{"look", "wallpaper"}, map[string]any{"catalog": pick("aurora", "dusk")})
			case 11:
				set(d, []string{"look", "theme", "iconTheme"}, pick("breeze", "papirus"))
			}
		}
		docs = append(docs, d)
	}
	type sel struct {
		ID      string `json:"id"`
		Pointer string `json:"pointer"`
	}
	type tc struct {
		Spec       map[string]any `json:"spec"`
		Selections []sel          `json:"selections"`
	}
	var out []tc
	for _, d := range docs {
		b, _ := json.Marshal(d)
		var s osspec.Spec
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		osspec.Default(&s)
		var ss []sel
		for _, x := range Selections(&s) {
			ss = append(ss, sel{x.ID, x.Pointer})
		}
		out = append(out, tc{d, ss})
	}
	if *webFixture {
		b, _ := json.Marshal(out)
		if err := os.WriteFile("../../web/src/solver/testdata/selections.json", b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
