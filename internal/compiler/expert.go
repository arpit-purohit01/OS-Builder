package compiler

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// expert maps Expert-mode content (forces human review, never "verified").
//
//   - overlays: uploaded tar archives extracted onto the root (mkosi ExtraTrees)
//   - postinstScripts: run in the image chroot after packages, without network
//     and with a 10-minute limit each
//   - mkosiDropins: raw mkosi configuration (keys validated at upload)
//   - extraRepos: apt sources with a pinned, uploaded signing key, at low
//     priority so they cannot replace base packages unless pinned
//   - packages: extra package names; apps.custom: uploaded .deb files
func (c *ctx) expert() {
	if c.s.Apps != nil {
		for i, d := range c.s.Apps.Custom {
			c.t.asset(fmt.Sprintf("mkosi.packages/custom-%02d.deb", i), d)
			if name := c.in.DebPackages[d]; name != "" {
				c.pkgs[name] = true
			}
		}
	}
	e := c.s.Expert
	if e == nil {
		return
	}
	for _, p := range e.Packages {
		c.pkgs[p] = true
	}
	for i, d := range e.Overlays {
		ext := c.in.AssetExt[d]
		if ext != ".tar.gz" && ext != ".tar.zst" && ext != ".tar.xz" {
			ext = ".tar"
		}
		name := fmt.Sprintf("expert/overlay-%02d%s", i, ext)
		c.t.asset(name, d)
		c.extraTrees = append(c.extraTrees, name)
	}
	for i, d := range e.PostinstScripts {
		c.t.asset(fmt.Sprintf("expert/postinst-%02d.sh", i), d)
	}
	if len(e.PostinstScripts) > 0 {
		c.postLate = append(c.postLate, customerScripts)
	}
	for i, d := range e.MkosiDropins {
		c.t.asset(fmt.Sprintf("mkosi.conf.d/90-expert-%02d.conf", i), d)
	}
	for i, r := range e.ExtraRepos {
		u, err := url.Parse(r.URL)
		if err != nil {
			continue // rejected by validation
		}
		suite := r.Suite
		if suite == "" {
			suite = c.base.Release
		}
		comps := r.Components
		if len(comps) == 0 {
			comps = []string{"main"}
		}
		sort.Strings(comps)
		key := fmt.Sprintf("etc/apt/keyrings/foundry-extra-%02d.asc", i)
		c.t.asset("mkosi.sandbox/"+key, r.KeyAsset)
		c.t.add(fmt.Sprintf("mkosi.sandbox/etc/apt/sources.list.d/foundry-extra-%02d.sources", i), 0o644, fmt.Sprintf(
			"# Expert extra repository (key %s)\nTypes: deb\nURIs: %s\nSuites: %s\nComponents: %s\nSigned-By: /%s\n",
			r.KeyFingerprint, r.URL, suite, strings.Join(comps, " "), key))
		// Low default priority: nothing from this repository replaces a base
		// package unless the pin says so.
		c.t.add(fmt.Sprintf("mkosi.sandbox/etc/apt/preferences.d/foundry-extra-%02d.pref", i), 0o644, fmt.Sprintf(
			"Package: *\nPin: origin %q\nPin-Priority: 100\n\nPackage: *\nPin: %s\nPin-Priority: 500\n", u.Hostname(), r.Pin))
	}
}

// customerScripts runs uploaded build scripts in the chroot. They see the
// image root, have no network (own network namespace, or no proxy when
// namespaces are unavailable) and are limited to 10 minutes each.
const customerScripts = `# Expert build scripts (customer content; reviewed before signing).
# Each runs as root in the image root, in its own mount and network namespace
# with the build's /work mounts (outputs, sources, package snapshot) removed,
# no network, and a 10 minute limit.
src=${SRCDIR:-/work/src}
shopt -s nullglob
scripts=("$src"/expert/postinst-*.sh)
if (( ${#scripts[@]} )); then
  stage=$(mktemp -d /var/tmp/foundry-scripts.XXXXXX)
  cp "${scripts[@]}" "$stage"/
  hide='for m in $(awk '"'"'$5 ~ "^/work" {print $5}'"'"' /proc/self/mountinfo | sort -r); do umount -l "$m" 2>/dev/null; done; [ -e /work/out ] && [ -n "$(ls -A /work/out 2>/dev/null)" ] && { echo "foundry: cannot isolate customer script" >&2; exit 97; }; exec /bin/bash "$0"'
  if unshare --mount --net true 2>/dev/null; then
    iso=(unshare --mount --net -- /bin/bash -c "$hide")
  elif unshare --net true 2>/dev/null; then
    echo "foundry: WARNING: no mount namespace for customer scripts; relying on the logged package snapshot digest"
    iso=(unshare --net -- /bin/bash)
  else
    echo "foundry: WARNING: no namespaces for customer scripts; network blocked by the build proxy only"
    iso=(env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY /bin/bash)
  fi
  for s in "$stage"/postinst-*.sh; do
    echo "foundry: running customer script $(basename "$s")"
    if ! timeout 600 "${iso[@]}" "$s"; then
      echo "foundry: customer script $(basename "$s") failed" >&2
      exit 1
    fi
  done
  rm -rf "$stage"
fi
`
