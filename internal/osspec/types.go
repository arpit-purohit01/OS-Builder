// Package osspec defines the Foundry OS specification: types, parsing,
// defaults, validation (schema + semantic), canonicalization and digests.
//
// Field-level documentation lives in docs/context/osspec-format.md.
package osspec

const (
	APIVersion = "foundry.dev/v1alpha1"
	Kind       = "OSSpec"
)

type Spec struct {
	APIVersion   string        `json:"apiVersion"`
	Kind         string        `json:"kind"`
	Metadata     Metadata      `json:"metadata"`
	Foundation   Foundation    `json:"foundation"`
	Look         *Look         `json:"look,omitempty"`
	Apps         *Apps         `json:"apps,omitempty"`
	Security     *Security     `json:"security,omitempty"`
	Access       *Access       `json:"access,omitempty"`
	Kernel       *Kernel       `json:"kernel,omitempty"`
	Memory       *Memory       `json:"memory,omitempty"`
	Ecosystem    *Ecosystem    `json:"ecosystem,omitempty"`
	Branding     *Branding     `json:"branding,omitempty"`
	Localization *Localization `json:"localization,omitempty"`
	Hardware     *Hardware     `json:"hardware,omitempty"`
	Expert       *Expert       `json:"expert,omitempty"`
}

type Metadata struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Version     string `json:"version"`
}

type Foundation struct {
	Base     string   `json:"base"`
	Arch     []string `json:"arch,omitempty"`
	Snapshot string   `json:"snapshot,omitempty"`
	Profile  string   `json:"profile"`
	BootMode string   `json:"bootMode,omitempty"`
}

type Look struct {
	Theme       *Theme       `json:"theme,omitempty"`
	Wallpaper   *Wallpaper   `json:"wallpaper,omitempty"`
	Layout      *Layout      `json:"layout,omitempty"`
	BootSplash  *BootSplash  `json:"bootSplash,omitempty"`
	LoginScreen *LoginScreen `json:"loginScreen,omitempty"`
}

type Theme struct {
	Mode      string `json:"mode,omitempty"`
	Accent    string `json:"accent,omitempty"`
	Radius    string `json:"radius,omitempty"`
	Font      string `json:"font,omitempty"`
	IconTheme string `json:"iconTheme,omitempty"`
}

type Wallpaper struct {
	Asset   string `json:"asset,omitempty"`
	Catalog string `json:"catalog,omitempty"`
}

type Layout struct {
	Preset string `json:"preset,omitempty"`
	Custom string `json:"custom,omitempty"`
}

type BootSplash struct {
	Logo       string `json:"logo,omitempty"`
	Background string `json:"background,omitempty"`
}

type LoginScreen struct {
	Background string `json:"background,omitempty"`
}

type Apps struct {
	Preinstalled []string `json:"preinstalled,omitempty"`
	System       []string `json:"system,omitempty"`
	Custom       []string `json:"custom,omitempty"`
}

type Security struct {
	Level          string    `json:"level,omitempty"`
	DiskEncryption string    `json:"diskEncryption,omitempty"`
	SecureBoot     string    `json:"secureBoot,omitempty"`
	Firewall       *Firewall `json:"firewall,omitempty"`
	USBStorage     string    `json:"usbStorage,omitempty"`
	AutoUpdates    *bool     `json:"autoUpdates,omitempty"`
}

type Firewall struct {
	Default string   `json:"default,omitempty"`
	Allow   []string `json:"allow,omitempty"`
}

type Access struct {
	RootModel  string `json:"rootModel,omitempty"`
	Users      *Users `json:"users,omitempty"`
	TTYConsole *bool  `json:"ttyConsole,omitempty"`
}

type Users struct {
	Mode       string `json:"mode,omitempty"`
	Predefined []User `json:"predefined,omitempty"`
}

type User struct {
	Name         string `json:"name"`
	Admin        bool   `json:"admin,omitempty"`
	PasswordHash string `json:"passwordHash,omitempty"`
}

type Kernel struct {
	Flavor       string   `json:"flavor,omitempty"`
	ExtraCmdline []string `json:"extraCmdline,omitempty"`
}

type Memory struct {
	Profile string `json:"profile,omitempty"`
}

type Ecosystem struct {
	Identity *Identity `json:"identity,omitempty"`
	AppStore *Toggle   `json:"appStore,omitempty"`
	Sync     *Sync     `json:"sync,omitempty"`
	Handoff  *Toggle   `json:"handoff,omitempty"`
	Fleet    *Fleet    `json:"fleet,omitempty"`
	Kiosk    *Kiosk    `json:"kiosk,omitempty"`
}

type Identity struct {
	Provider string `json:"provider,omitempty"`
	Issuer   string `json:"issuer,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	Domain   string `json:"domain,omitempty"`
}

type Toggle struct {
	Enabled bool `json:"enabled"`
}

type Sync struct {
	Provider string `json:"provider,omitempty"`
	URL      string `json:"url,omitempty"`
}

type Fleet struct {
	Enroll bool   `json:"enroll"`
	Group  string `json:"group,omitempty"`
}

type Kiosk struct {
	URL              string   `json:"url,omitempty"`
	IdleResetMinutes int      `json:"idleResetMinutes,omitempty"`
	AllowedDomains   []string `json:"allowedDomains,omitempty"`
}

type Branding struct {
	OSName     string `json:"osName,omitempty"`
	OSID       string `json:"osId,omitempty"`
	HomeURL    string `json:"homeUrl,omitempty"`
	SupportURL string `json:"supportUrl,omitempty"`
}

type Localization struct {
	Locale   string `json:"locale,omitempty"`
	Keymap   string `json:"keymap,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

type Hardware struct {
	Targets       []string `json:"targets,omitempty"`
	ExtraFirmware []string `json:"extraFirmware,omitempty"`
}

type ExtraRepo struct {
	URL            string   `json:"url"`
	KeyFingerprint string   `json:"keyFingerprint"`
	KeyAsset       string   `json:"keyAsset"`
	Pin            string   `json:"pin"`
	Suite          string   `json:"suite,omitempty"`
	Components     []string `json:"components,omitempty"`
}

type Expert struct {
	Packages        []string    `json:"packages,omitempty"`
	Overlays        []string    `json:"overlays,omitempty"`
	PostinstScripts []string    `json:"postinstScripts,omitempty"`
	ExtraRepos      []ExtraRepo `json:"extraRepos,omitempty"`
	MkosiDropins    []string    `json:"mkosiDropins,omitempty"`
}

// HasExpert reports whether any expert content is present (forces review,
// forbids the verified trust tier).
func (s *Spec) HasExpert() bool {
	e := s.Expert
	return e != nil && (len(e.Packages) > 0 || len(e.Overlays) > 0 || len(e.PostinstScripts) > 0 || len(e.ExtraRepos) > 0 || len(e.MkosiDropins) > 0) ||
		(s.Apps != nil && len(s.Apps.Custom) > 0) ||
		(s.Kernel != nil && len(s.Kernel.ExtraCmdline) > 0)
}

// AssetDigests lists every uploaded asset referenced by the spec.
func (s *Spec) AssetDigests() []string {
	var out []string
	add := func(v string) {
		if v != "" {
			out = append(out, v)
		}
	}
	if l := s.Look; l != nil {
		if l.Wallpaper != nil {
			add(l.Wallpaper.Asset)
		}
		if l.Layout != nil {
			add(l.Layout.Custom)
		}
		if l.BootSplash != nil {
			add(l.BootSplash.Logo)
		}
		if l.LoginScreen != nil {
			add(l.LoginScreen.Background)
		}
	}
	if s.Apps != nil {
		out = append(out, s.Apps.Custom...)
	}
	if e := s.Expert; e != nil {
		out = append(out, e.Overlays...)
		out = append(out, e.PostinstScripts...)
		out = append(out, e.MkosiDropins...)
		for _, r := range e.ExtraRepos {
			out = append(out, r.KeyAsset)
		}
	}
	return out
}
