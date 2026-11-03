package compiler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foundry.dev/foundry/internal/catalog"
	"foundry.dev/foundry/internal/osspec"
	"foundry.dev/foundry/internal/specsvc"
)

func TestCustomLayout(t *testing.T) {
	r := root(t)
	cat, err := catalog.LoadDir(filepath.Join(r, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := osspec.ParseLayout([]byte(`{"version":1,"panels":[
		{"edge":"left","style":"dock","size":56,"autohide":true,"items":["launcher","favorites","tasks","trash"]},
		{"edge":"top","style":"bar","size":28,"items":["desktops","spacer","clock","tray"]}],
		"favorites":["org.mozilla.firefox.desktop","org.libreoffice.LibreOffice.writer.desktop"]}`))
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("ab", 32)
	for _, tc := range []struct{ spec, file string }{
		{"office-laptop.yaml", "mkosi.extra/usr/share/plasma/shells/org.kde.plasma.desktop/contents/updates/foundry-defaults.js"},
		{"school-lab.yaml", "mkosi.extra/etc/dconf/db/local.d/10-foundry"},
	} {
		data, _ := os.ReadFile(filepath.Join(r, "test/golden/specs", tc.spec))
		pol := specsvc.DefaultPolicy()
		pol.ExpertAllowed = true
		v, err := specsvc.Validate(data, cat, pol)
		if err != nil {
			t.Fatal(err)
		}
		v.Spec.Look.Layout.Preset, v.Spec.Look.Layout.Custom = "", digest
		out, err := specsvc.Compile(v, cat.Version, GoldenPlatform, specsvc.Assets{Layouts: map[string]*osspec.LayoutDoc{digest: doc}})
		if err != nil {
			t.Fatal(err)
		}
		got := string(out.Tree.Files[tc.file].Data)
		switch tc.spec {
		case "office-laptop.yaml":
			for _, want := range []string{"p.remove()", `p0.location = "left";`, `p0.hiding = "autohide";`, `p0.lengthMode = "fit";`,
				`"applications:org.mozilla.firefox.desktop"`, `p1.addWidget("org.kde.plasma.pager");`, `p0.addWidget("org.kde.plasma.trash");`} {
				if !strings.Contains(got, want) {
					t.Errorf("KDE layout lacks %s:\n%s", want, got)
				}
			}
			if strings.Count(got, `addWidget("org.kde.plasma.icontasks")`) != 1 {
				t.Error("favorites and tasks should share one task manager")
			}
		case "school-lab.yaml":
			for _, want := range []string{"dock-position='LEFT'", "dock-fixed=false", "dash-max-icon-size=42",
				"favorite-apps=['org.mozilla.firefox.desktop', 'org.libreoffice.LibreOffice.writer.desktop']", "enabled-extensions=['ubuntu-dock@ubuntu.com']"} {
				if !strings.Contains(got, want) {
					t.Errorf("GNOME layout lacks %s:\n%s", want, got)
				}
			}
			if strings.Count(got, "[org/gnome/shell]") != 1 {
				t.Errorf("duplicate [org/gnome/shell] section:\n%s", got)
			}
		}
	}
}
