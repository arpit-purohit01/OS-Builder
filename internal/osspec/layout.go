package osspec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// LayoutDoc is the custom desktop layout produced by the builder's layout
// editor and uploaded as a "layout" asset (look.layout.custom = its digest).
//
//	{"version": 1,
//	 "panels": [{"edge": "bottom", "style": "dock", "size": 48, "autohide": false,
//	             "items": ["launcher", "favorites", "tasks", "tray", "clock"]}],
//	 "favorites": ["org.mozilla.firefox.desktop"]}
type LayoutDoc struct {
	Version   int           `json:"version"`
	Panels    []LayoutPanel `json:"panels"`
	Favorites []string      `json:"favorites,omitempty"`
}

type LayoutPanel struct {
	Edge     string   `json:"edge"`  // top | bottom | left | right
	Style    string   `json:"style"` // dock (centered, fits content) | bar (full width)
	Size     int      `json:"size"`  // px, 24–96
	Autohide bool     `json:"autohide,omitempty"`
	Items    []string `json:"items"`
}

// LayoutItems are the widgets a panel may hold.
var LayoutItems = map[string]bool{"launcher": true, "favorites": true, "tasks": true, "tray": true, "clock": true, "search": true, "spacer": true, "desktops": true, "trash": true}

var desktopIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}\.desktop$`)

// ParseLayout decodes and strictly validates a layout document.
func ParseLayout(b []byte) (*LayoutDoc, error) {
	if len(b) > 64<<10 {
		return nil, fmt.Errorf("layout: document too large")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var l LayoutDoc
	if err := d.Decode(&l); err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	if l.Version != 1 {
		return nil, fmt.Errorf("layout: unsupported version %d", l.Version)
	}
	if len(l.Panels) == 0 || len(l.Panels) > 4 {
		return nil, fmt.Errorf("layout: 1 to 4 panels are required")
	}
	edges := map[string]bool{}
	for i, p := range l.Panels {
		switch p.Edge {
		case "top", "bottom", "left", "right":
		default:
			return nil, fmt.Errorf("layout: panel %d: unknown edge %q", i+1, p.Edge)
		}
		if edges[p.Edge] {
			return nil, fmt.Errorf("layout: two panels on the %s edge", p.Edge)
		}
		edges[p.Edge] = true
		if p.Style != "dock" && p.Style != "bar" {
			return nil, fmt.Errorf("layout: panel %d: style must be dock or bar", i+1)
		}
		if p.Size < 24 || p.Size > 96 {
			return nil, fmt.Errorf("layout: panel %d: size must be 24–96", i+1)
		}
		if len(p.Items) == 0 || len(p.Items) > 12 {
			return nil, fmt.Errorf("layout: panel %d: 1 to 12 items", i+1)
		}
		for _, it := range p.Items {
			if !LayoutItems[it] {
				return nil, fmt.Errorf("layout: panel %d: unknown item %q", i+1, it)
			}
		}
	}
	if len(l.Favorites) > 24 {
		return nil, fmt.Errorf("layout: at most 24 favorites")
	}
	for _, f := range l.Favorites {
		if !desktopIDRe.MatchString(f) {
			return nil, fmt.Errorf("layout: invalid application id %q", f)
		}
	}
	return &l, nil
}
