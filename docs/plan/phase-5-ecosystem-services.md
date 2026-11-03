# Phase 5 — Ecosystem services (10 weeks)

## Goal
Turn a fleet of individual machines into a platform: users sign in with their organization identity on
any device, apps come from a curated per-tenant store, files and settings follow the user, admins push
policies, and devices work together (handoff, shared clipboard). Built from proven open components,
branded as the customer's OS.

## Principles
- Integrate, don't reinvent: OIDC/SSSD, Flatpak, Nextcloud/WebDAV, KDE Connect/GSConnect, Matrix.
- Every service is optional per spec (`ecosystem.*`), and can point at the customer's own server ("bring your own endpoint").
- Hosted services are multi-tenant with hard tenant isolation (separate buckets/namespaces, per-tenant keys).

## Tasks

| ID | Task | Detail | Design doc |
|---|---|---|---|
| P5-01 | Device sign-in | Spec `ecosystem.identity`: `local`, `oidc` (Entra ID, Google, Keycloak, Okta) or `ldap/ad`. Implementation: SSSD (AD/LDAP) or an OIDC PAM/NSS module with offline credential cache; greeter shows org branding; first login creates home (systemd-homed optional, encrypted) | fleet-and-device-agent.md §Identity |
| P5-02 | App store | Per-tenant Flatpak remote (OSTree repo served from object storage + CDN) containing curated apps mirrored from Flathub (license-checked) and customer apps (reviewed via Phase 3 pipeline). Signed with tenant GPG key; `/etc/flatpak/remotes.d/<tenant>.flatpakrepo` baked into image; store UI = GNOME Software/Discover pointed at the remote, branded | security-review-pipeline skill |
| P5-03 | App allow/deny policy | Policy says which apps are required (auto-install), allowed, or blocked; agent enforces (uninstall blocked, install required); kiosk: only one app | fleet-and-device-agent.md §Policy |
| P5-04 | Preinstalled apps offline | Build bakes a sideload repo of preinstalled Flatpaks into `/usr/share/foundry/flatpak-sideload`; first-boot unit installs from it with no network | build-pipeline.md |
| P5-05 | Sync | Option A hosted: Nextcloud (or any WebDAV) per tenant, desktop client preconfigured with SSO; Option B BYO endpoint URL. Settings sync for theme/wallpaper/browser profile via the same backend | — |
| P5-06 | Device handoff | KDE Connect (KDE) / GSConnect (GNOME) preinstalled, pairing restricted to devices with same org cert (agent provisions trusted certs) | fleet-and-device-agent.md |
| P5-07 | Messaging (optional) | Matrix homeserver per tenant (Synapse/Conduit) + Element preinstalled with SSO; or BYO | — |
| P5-08 | Policy engine | Declarative policy documents (JSON, signed) → agent renders to dconf locks (GNOME), Plasma Kiosk `[$i]` immutable keys (KDE), polkit rules, nftables, usbguard rules, browser policies (`/etc/chromium/policies/managed/*.json`, Firefox `policies.json`) | fleet-and-device-agent.md §Policy |
| P5-09 | Kiosk management | Remote URL change, scheduled reboot, screenshot on demand (opt-in, logged), idle reset (clear browser profile), watchdog restarts app | fleet-and-device-agent.md |
| P5-10 | Remote support (opt-in) | User-consented remote view/control via WebRTC (wayvnc on wlroots/cage; KRdp on KDE; GNOME Remote Desktop) relayed through TURN; session recorded in audit log | security-threat-model.md |
| P5-11 | Notifications | Admin → device banners/notifications via agent → `notify-send` on the user session bus | — |
| P5-12 | Reports | Compliance report per device (encryption on, SB on, attested, release current, CVEs open) exportable CSV/PDF | data-model.md |

## Acceptance tests
1. Sign in with Entra ID / Google on two devices; offline login works after first online login.
2. Policy blocks app X: user cannot launch; uninstall happens within one heartbeat.
3. App store only shows tenant-approved apps; installing an unsigned Flatpak from another remote is blocked by policy.
4. Sync conflicts handled (last-writer-wins with conflict copy) — verified by the sync product's own semantics.
5. Remote support cannot start without on-screen user consent; audit entry created.

## Exit gate
One pilot customer runs ≥ 50 devices with SSO + app store + policies for 4 weeks with no P1 incidents.
