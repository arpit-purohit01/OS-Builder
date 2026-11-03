package compiler_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"foundry.dev/foundry/internal/catalog"
	"foundry.dev/foundry/internal/compiler"
	"foundry.dev/foundry/internal/specsvc"
)

var update = flag.Bool("update", false, "rewrite golden files")

func root(t *testing.T) string {
	d, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		d = filepath.Dir(d)
	}
	t.Fatal("no root")
	return ""
}

// GoldenPlatform is the fixed platform configuration used for goldens.
var GoldenPlatform = compiler.Platform{
	Tenant: "acme", FleetURL: "https://fleet.foundry.test", UpdateURL: "https://updates.foundry.test", SyncURL: "https://sync.foundry.test",
}

type meta struct {
	Digest   string   `json:"treeDigest"`
	Packages []string `json:"packages"`
	Labels   compiler.Labels
	Cmdline  []string `json:"cmdline"`
	Warnings []string `json:"warnings"`
}

func compileSpec(t *testing.T, cat *catalog.Catalog, path string) *compiler.Output {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pol := specsvc.DefaultPolicy()
	pol.ExpertAllowed = true
	v, err := specsvc.Validate(data, cat, pol)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out, err := specsvc.Compile(v, cat.Version, GoldenPlatform, specsvc.Assets{})
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

func TestGolden(t *testing.T) {
	r := root(t)
	cat, err := catalog.LoadDir(filepath.Join(r, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	specs, _ := filepath.Glob(filepath.Join(r, "test/golden/specs/*.yaml"))
	if len(specs) == 0 {
		t.Fatal("no golden specs")
	}
	for _, sp := range specs {
		name := strings.TrimSuffix(filepath.Base(sp), ".yaml")
		t.Run(name, func(t *testing.T) {
			out := compileSpec(t, cat, sp)
			// Determinism: a second compile is byte-identical.
			if again := compileSpec(t, cat, sp); again.Tree.Digest() != out.Tree.Digest() {
				t.Fatal("compile is not deterministic")
			}
			dir := filepath.Join(r, "test/golden/out", name)
			m := meta{Digest: out.Tree.Digest(), Packages: out.Packages, Labels: out.Labels, Cmdline: out.Cmdline, Warnings: out.Warnings}
			mb, _ := json.MarshalIndent(m, "", "  ")
			want := map[string][]byte{"_meta.json": append(mb, '\n')}
			for _, p := range out.Tree.Paths() {
				f := out.Tree.Files[p]
				switch {
				case f.Symlink != "":
					want[p+".symlink"] = []byte(f.Symlink + "\n")
				case f.AssetRef != "":
					want[p+".ref"] = []byte("asset " + f.AssetRef + "\n")
				case f.ComponentRef != "":
					want[p+".ref"] = []byte("component " + f.ComponentRef + "\n")
				default:
					want[p] = f.Data
				}
			}
			if *update {
				os.RemoveAll(dir)
				for p, b := range want {
					dst := filepath.Join(dir, p)
					os.MkdirAll(filepath.Dir(dst), 0o755)
					if err := os.WriteFile(dst, b, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			var got []string
			filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					rel, _ := filepath.Rel(dir, p)
					got = append(got, filepath.ToSlash(rel))
				}
				return nil
			})
			sort.Strings(got)
			var wantKeys []string
			for k := range want {
				wantKeys = append(wantKeys, k)
			}
			sort.Strings(wantKeys)
			if strings.Join(got, "\n") != strings.Join(wantKeys, "\n") {
				t.Fatalf("golden file set differs for %s; run `make golden-update` and review.\nwant:\n%s\ngot:\n%s", name, strings.Join(wantKeys, "\n"), strings.Join(got, "\n"))
			}
			for _, k := range wantKeys {
				b, _ := os.ReadFile(filepath.Join(dir, k))
				if !bytes.Equal(b, want[k]) {
					t.Errorf("%s/%s differs from golden; run `make golden-update` and review the diff", name, k)
				}
			}
		})
	}
}
