// Package scan produces the security view of a build: an SPDX 2.3 SBOM
// from the package manifest, findings from rules over the spec and the
// build's contents, a risk score and the trust tier that decides whether a
// human review is required (docs/context/security-threat-model.md).
package scan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Package is one installed distribution package.
type Package struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"architecture"`
	Source  string `json:"source,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// ParseManifest reads mkosi's JSON manifest (packages list).
func ParseManifest(b []byte) ([]Package, error) {
	var m struct {
		Packages []Package `json:"packages"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("scan: manifest: %w", err)
	}
	sort.Slice(m.Packages, func(i, j int) bool { return m.Packages[i].Name < m.Packages[j].Name })
	return m.Packages, nil
}

// SBOMInput describes the image for the SBOM document.
type SBOMInput struct {
	BuildID     string
	OSName      string
	OSID        string
	Version     string
	Distro      string // ubuntu | debian
	Release     string // noble | trixie
	Arch        string
	SpecDigest  string
	Roothash    string
	SourceEpoch int64
	Packages    []Package
}

// SBOM renders an SPDX 2.3 JSON document (deterministic for equal inputs).
func SBOM(in SBOMInput) ([]byte, error) {
	type ref struct {
		Category string `json:"referenceCategory"`
		Type     string `json:"referenceType"`
		Locator  string `json:"referenceLocator"`
	}
	type checksum struct {
		Algorithm string `json:"algorithm"`
		Value     string `json:"checksumValue"`
	}
	type pkg struct {
		Name             string     `json:"name"`
		SPDXID           string     `json:"SPDXID"`
		Version          string     `json:"versionInfo,omitempty"`
		Supplier         string     `json:"supplier,omitempty"`
		Download         string     `json:"downloadLocation"`
		FilesAnalyzed    bool       `json:"filesAnalyzed"`
		PrimaryPurpose   string     `json:"primaryPackagePurpose,omitempty"`
		Checksums        []checksum `json:"checksums,omitempty"`
		ExternalRefs     []ref      `json:"externalRefs,omitempty"`
		LicenseConcluded string     `json:"licenseConcluded"`
		LicenseDeclared  string     `json:"licenseDeclared"`
		Copyright        string     `json:"copyrightText"`
	}
	type rel struct {
		A    string `json:"spdxElementId"`
		Type string `json:"relationshipType"`
		B    string `json:"relatedSpdxElement"`
	}
	osPkg := pkg{Name: in.OSName, SPDXID: "SPDXRef-OS", Version: in.Version, Download: "NOASSERTION", PrimaryPurpose: "OPERATING-SYSTEM",
		LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION", Copyright: "NOASSERTION"}
	if in.Roothash != "" {
		osPkg.Checksums = []checksum{{"SHA256", in.Roothash}}
	}
	pkgs := []pkg{osPkg}
	rels := []rel{{"SPDXRef-DOCUMENT", "DESCRIBES", "SPDXRef-OS"}}
	supplier := map[string]string{"ubuntu": "Organization: Canonical Ltd.", "debian": "Organization: Debian"}[in.Distro]
	for _, p := range in.Packages {
		id := "SPDXRef-Package-" + spdxSafe(p.Type+"-"+p.Name+"-"+p.Arch)
		purl := fmt.Sprintf("pkg:%s/%s/%s@%s?arch=%s", purlType(p.Type), in.Distro, p.Name, purlEscape(p.Version), p.Arch)
		if in.Release != "" {
			purl += "&distro=" + in.Distro + "-" + in.Release
		}
		pkgs = append(pkgs, pkg{Name: p.Name, SPDXID: id, Version: p.Version, Supplier: supplier, Download: "NOASSERTION",
			PrimaryPurpose: "LIBRARY", ExternalRefs: []ref{{"PACKAGE-MANAGER", "purl", purl}},
			LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION", Copyright: "NOASSERTION"})
		rels = append(rels, rel{"SPDXRef-OS", "CONTAINS", id})
	}
	doc := map[string]any{
		"spdxVersion":       "SPDX-2.3",
		"dataLicense":       "CC0-1.0",
		"SPDXID":            "SPDXRef-DOCUMENT",
		"name":              in.OSID + "_" + in.Version,
		"documentNamespace": "https://foundry.dev/spdx/" + in.OSID + "/" + in.Version + "/" + in.BuildID,
		"creationInfo": map[string]any{
			"created":  time.Unix(in.SourceEpoch, 0).UTC().Format(time.RFC3339),
			"creators": []string{"Tool: foundry-scanner-1", "Organization: Foundry"},
			"comment":  "spec " + in.SpecDigest,
		},
		"packages":      pkgs,
		"relationships": rels,
	}
	return json.MarshalIndent(doc, "", " ")
}

func purlType(t string) string {
	switch t {
	case "deb", "rpm":
		return t
	}
	return "generic"
}

func purlEscape(v string) string {
	return strings.NewReplacer(":", "%3A", "+", "%2B", "~", "%7E").Replace(v)
}

func spdxSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, s)
}
