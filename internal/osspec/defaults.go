package osspec

// Default fills every optional field with its documented default so the
// canonical form is explicit. Defaults must never change meaning of
// existing specs (that would change their digest) — add new fields only.
func Default(s *Spec) {
	f := &s.Foundation
	if f.Base == "" {
		f.Base = "ubuntu-24.04"
	}
	if len(f.Arch) == 0 {
		f.Arch = []string{"x86_64"}
	}
	if f.BootMode == "" {
		f.BootMode = "uefi"
	}
	kiosk := f.Profile == "kiosk-cage" || f.Profile == "thin-client"
	desktop := f.Profile == "desktop-kde" || f.Profile == "desktop-gnome"

	if s.Look == nil {
		s.Look = &Look{}
	}
	if s.Look.Theme == nil {
		s.Look.Theme = &Theme{}
	}
	t := s.Look.Theme
	def(&t.Mode, "auto")
	def(&t.Accent, "#4F46E5")
	def(&t.Radius, "medium")
	def(&t.Font, "inter")
	if s.Look.Layout == nil {
		s.Look.Layout = &Layout{}
	}
	if s.Look.Layout.Preset == "" && s.Look.Layout.Custom == "" {
		switch {
		case desktop:
			s.Look.Layout.Preset = "dock-bottom"
		default:
			s.Look.Layout.Preset = "none"
		}
	}
	if s.Look.BootSplash == nil {
		s.Look.BootSplash = &BootSplash{}
	}
	def(&s.Look.BootSplash.Background, "#0B1020")
	if s.Look.BootSplash.HideMenu == nil {
		v := true
		s.Look.BootSplash.HideMenu = &v
	}

	if s.Apps == nil {
		s.Apps = &Apps{}
	}
	if s.Security == nil {
		s.Security = &Security{}
	}
	sec := s.Security
	def(&sec.Level, "balanced")
	def(&sec.DiskEncryption, "tpm2")
	def(&sec.SecureBoot, "tenant-keys")
	def(&sec.USBStorage, "allow")
	if sec.Firewall == nil {
		sec.Firewall = &Firewall{}
	}
	def(&sec.Firewall.Default, "deny-inbound")
	if sec.AutoUpdates == nil {
		v := true
		sec.AutoUpdates = &v
	}

	if s.Access == nil {
		s.Access = &Access{}
	}
	a := s.Access
	if kiosk {
		def(&a.RootModel, "locked")
	} else {
		def(&a.RootModel, "admin-password")
	}
	if a.Users == nil {
		a.Users = &Users{}
	}
	if kiosk {
		def(&a.Users.Mode, "autologin-kiosk")
	} else {
		def(&a.Users.Mode, "firstboot")
	}
	if a.TTYConsole == nil {
		v := false
		a.TTYConsole = &v
	}

	if s.Kernel == nil {
		s.Kernel = &Kernel{}
	}
	def(&s.Kernel.Flavor, "standard")
	if s.Memory == nil {
		s.Memory = &Memory{}
	}
	def(&s.Memory.Profile, "auto")

	if s.Ecosystem == nil {
		s.Ecosystem = &Ecosystem{}
	}
	e := s.Ecosystem
	if e.Identity == nil {
		e.Identity = &Identity{}
	}
	def(&e.Identity.Provider, "local")
	if e.AppStore == nil {
		e.AppStore = &Toggle{}
	}
	if e.Sync == nil {
		e.Sync = &Sync{}
	}
	def(&e.Sync.Provider, "none")
	if e.Handoff == nil {
		e.Handoff = &Toggle{}
	}
	if e.Fleet == nil {
		e.Fleet = &Fleet{}
	}

	if s.Branding == nil {
		s.Branding = &Branding{}
	}
	def(&s.Branding.OSID, s.Metadata.Name)
	name := s.Metadata.DisplayName
	if name == "" {
		name = s.Metadata.Name
	}
	def(&s.Branding.OSName, name)

	if s.Localization == nil {
		s.Localization = &Localization{}
	}
	def(&s.Localization.Locale, "en_US.UTF-8")
	def(&s.Localization.Keymap, "us")
	def(&s.Localization.Timezone, "UTC")

	if s.Hardware == nil {
		s.Hardware = &Hardware{}
	}
	if len(s.Hardware.Targets) == 0 {
		s.Hardware.Targets = []string{"generic-uefi-" + f.Arch[0]}
	}
}

func def(p *string, v string) {
	if *p == "" {
		*p = v
	}
}
