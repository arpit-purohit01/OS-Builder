package osspec

import "testing"

func TestParseLayout(t *testing.T) {
	ok := `{"version":1,"panels":[{"edge":"bottom","style":"dock","size":48,"items":["launcher","favorites","tasks"]},{"edge":"top","style":"bar","size":28,"items":["desktops","spacer","clock","tray"]}],"favorites":["org.mozilla.firefox.desktop"]}`
	if _, err := ParseLayout([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"unknown field": `{"version":1,"panels":[{"edge":"bottom","style":"dock","size":48,"items":["tasks"],"script":"rm -rf /"}]}`,
		"dup edge":      `{"version":1,"panels":[{"edge":"top","style":"bar","size":28,"items":["clock"]},{"edge":"top","style":"bar","size":28,"items":["tray"]}]}`,
		"bad item":      `{"version":1,"panels":[{"edge":"top","style":"bar","size":28,"items":["org.evil.widget"]}]}`,
		"bad fav":       `{"version":1,"panels":[{"edge":"top","style":"bar","size":28,"items":["clock"]}],"favorites":["\";exec(\"x\");\".desktop"]}`,
		"size":          `{"version":1,"panels":[{"edge":"top","style":"bar","size":500,"items":["clock"]}]}`,
		"version":       `{"version":2,"panels":[{"edge":"top","style":"bar","size":28,"items":["clock"]}]}`,
	} {
		if _, err := ParseLayout([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
