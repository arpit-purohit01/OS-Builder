# Foundry OS Builder

> **Foundry** is a B2B no-code platform that turns an **OS specification** (YAML/JSON edited through a GUI) into a **signed, bootable, updatable Linux operating system image**, then installs, updates, and manages it on fleets of devices.

Built on Debian 13 "trixie" using `mkosi`, `systemd-repart`, `ukify`, and `systemd-sysupdate`.

## 🚀 Quick Start with VS Code DevContainers (Recommended)

This project includes a complete **DevContainer** configuration that provides a ready-to-use Linux development environment with:
- Go 1.25+, QEMU/KVM, PostgreSQL 16
- mkosi, systemd-ukify, sbsigntool, swtpm
- All system dependencies pre-installed
- KVM device access for fast boot testing

### Prerequisites
- VS Code with **Dev Containers** extension
- Docker Desktop (with "Use Rosetta" on Apple Silicon)
- **Windows**: Enable WSL2 and Virtualization in BIOS
- **Linux**: `/dev/kvm` access (user in `kvm` group)

### One-Click Setup
1. Open this folder in VS Code
2. Press `F1` → **Dev Containers: Reopen in Container**
3. Wait for build (~5-10 min first time)
4. Run tests: `make e2e`

---

## 🛠 Manual Setup (Linux/WSL2)

### Install Dependencies
```bash
# Ubuntu 24.04 / Debian 13
sudo apt update && sudo apt install -y \
  golang-go qemu-system-x86 qemu-utils ovmf swtpm swtpm-tools \
  systemd-boot-efi systemd-ukify sbsigntool efitools openssl \
  mtools dosfstools cryptsetup-bin e2fsprogs fdisk zstd xz-utils \
  postgresql-16 postgresql-client-16 nodejs npm shellcheck jq \
  libvirt-daemon-system libvirt-clients bridge-utils

# Install mkosi (pinned version)
pip3 install "mkosi==24.*"

# Install Go tools
go install golang.org/x/tools/cmd/goimports@latest
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

### Start Services
```bash
# PostgreSQL
sudo systemctl start postgresql
sudo -u postgres psql -c "CREATE DATABASE foundry;"

# libvirtd (for KVM)
sudo systemctl start libvirtd
sudo usermod -aG kvm,libvirt $USER
newgrp kvm
```

### Build & Test
```bash
# Build all binaries
make build components

# Create tools tree (mkosi cache)
make tools-tree

# Start dev stack (API, scheduler, worker, signer, previewd)
make dev-up

# In another terminal: login and create org
bin/fctl login -url http://127.0.0.1:8080 -email you@example.com -create-org "MyOrg:myorg"

# Run full e2e test (~5 min with KVM, ~30 min TCG)
make e2e
```

---

## 📋 Available Make Targets

| Target | Description |
|--------|-------------|
| `make build` | Build all Go binaries |
| `make components` | Build static agent/installer binaries |
| `make test` | Unit + integration tests (requires Postgres) |
| `make test-race` | Tests with race detector |
| `make lint` | go vet, gofmt, shellcheck |
| `make golden` | Compile example specs, diff against goldens |
| `make golden-update` | Regenerate golden outputs |
| `make tools-tree` | Create mkosi tool cache |
| `make dev-up` | Start local dev stack |
| `make dev-down` | Stop dev stack |
| `make e2e` | Full end-to-end pipeline test |
| `make image SPEC=...` | Build single image (process sandbox) |
| `make sign-image IMAGE=...` | Sign image with dev keys |
| `make boot-test IMAGE=... MATRIX=...` | Boot test in QEMU |
| `make phase0` | Phase 0 acceptance suite |
| `make phase1` | Phase 1 acceptance (test + e2e) |
| `make test-crash PROJECT=...` | Acceptance 5: worker crash re-lease |
| `make test-repro PROJECT=...` | Acceptance 4: reproducibility |
| `make web` | Build React frontend |
| `make web-test` | Frontend tests |

---

## 🏗 Project Structure

```
cmd/                    # Entry points (binaries)
  api/                  # REST API server
  scheduler/            # Build scheduler (DRR + rendezvous)
  worker-agent/         # KVM host agent
  builder/              # Build VM process (mkosi)
  scanner/              # SBOM, CVE, YARA, risk scoring
  tester/               # QEMU boot test harness
  signer/               # Isolated signing service
  foundry-agent/        # On-device agent
  fctl/                 # CLI tool

internal/               # Private packages
  osspec/               # Spec types, validation, canonicalization
  catalog/              # Option catalog, CNF/SAT solver
  compiler/             # Spec → mkosi tree (pure function)
  dag/                  # Build stage graph, cache keys
  cache/                # Content-addressed store, W-TinyLFU
  store/                # Postgres repos, build FSM
  queue/                # River job queue
  sched/                # Scheduler algorithms
  blob/                 # S3-compatible artifact store
  tuf/                  # TUF metadata
  attest/               # TPM quote verification

web/                    # React + TypeScript builder UI
images/                 # mkosi base images, profiles, modules
catalog/                # YAML option catalog
deploy/                 # Terraform, Helm, Ansible
docs/                   # Plans, context, schemas, examples
scripts/                # Dev/test automation
test/                   # Golden files, boot scenarios, e2e
```

---

## 🧪 Phase Status

| Phase | Name | Status | Exit Gate |
|-------|------|--------|-----------|
| **0** | Foundations | ✅ **Complete** | 6/6 boot tests pass (UEFI, SB, TPM, A/B) |
| **1** | MVP Image Pipeline | 🔄 **Code Complete** | 3 pilot specs, 2 devices each, 7-day reproducibility |
| **2** | Builder & Customization | 📋 Planned | Non-technical user builds/installs in <30 min |
| **3** | Security & Trust | 📋 Planned | Red-team blocked, tenant HSM keys |
| **4** | Updates & Fleet | 📋 Planned | 100-device pilot, auto-rollback proven |

### Phase 1 Remaining Verification (Run in DevContainer)
- [ ] **Acceptance 5**: Worker crash re-lease + orphan reaper (`make test-crash`)
- [ ] **Acceptance 2**: Example specs boot matrix (kiosk, school-lab, office-laptop)
- [ ] **Acceptance 3**: Build time targets for desktop profiles
- [ ] **P1-13**: All output formats (img.xz, qcow2, vmdk, ova, iso)
- [ ] **P1-11**: S3 artifact store vs real MinIO
- [ ] **VM Sandbox**: Full build inside QEMU builder VM (`FOUNDRY_SANDBOX=vm`)

---

## 🔑 Key Concepts

### OS Specification (OSSpec)
Single source of truth for an OS. Edited via GUI, compiled to mkosi tree.

```yaml
apiVersion: foundry.dev/v1alpha1
kind: OSSpec
metadata:
  name: my-kiosk
  version: 1.0.0
foundation:
  base: debian-13
  profile: kiosk-cage
security:
  diskEncryption: tpm2
  secureBoot: tenant-keys
apps:
  preinstalled: [flatpak:org.mozilla.firefox]
```

### Build Flow
1. **Spec** → validation (schema + semantic + SAT + policy)
2. **Compiler** → pure function → mkosi tree (deterministic)
3. **Worker** → ephemeral KVM VM → mkosi build → artifacts
4. **Scanner** → SBOM, CVE, YARA, risk score
5. **Tester** → QEMU boot test (UEFI/SB/TPM matrix)
6. **Signer** → signs UKI, bootloader, verity, manifest
7. **Publish** → content-addressed artifacts → signed downloads

### Reproducibility
Two builds of the same spec digest **must** produce identical `/usr` dm-verity root hash.
- `SOURCE_DATE_EPOCH` from base snapshot
- Pinned mirrors (snapshot.debian.org)
- Pinned tool versions (deploy/versions.lock)

---

## 🔧 Development Workflow

### Adding a Spec Field
Use the `osspec-schema-change` skill (`.claude/skills/osspec-schema-change/`):
1. Update JSON Schema (`schemas/osspec.v1alpha1.schema.json`)
2. Update Go types (`internal/osspec/`)
3. Add defaults, validation, compiler mapping
4. Add golden test (`test/golden/`)
5. Update catalog/UI/docs

### Running Tests
```bash
# Unit tests (fast, no KVM needed)
make test

# Integration tests (needs Postgres)
FOUNDRY_REQUIRE_PG=1 make test

# Full e2e (needs KVM for speed)
make e2e

# Specific acceptance tests
make test-crash PROJECT=prj_xxx   # Worker crash
make test-repro PROJECT=prj_xxx   # Reproducibility
```

### Debugging
```bash
# View dev stack logs
tail -f out/dev/logs/*.log

# Inspect build in progress
bin/fctl logs <build-id>
bin/fctl status <build-id>

# Database inspection
psql postgres://postgres@127.0.0.1:5432/foundry
```

---

## 📚 Documentation

| Document | Description |
|----------|-------------|
| `docs/context/architecture.md` | System architecture, component map, flows |
| `docs/context/osspec-format.md` | Complete spec format reference |
| `docs/context/build-pipeline.md` | Build stages, DAG, sandbox, cache |
| `docs/context/boot-firmware-and-disk.md` | UEFI, Secure Boot, TPM, partitions |
| `docs/context/security-threat-model.md` | STRIDE threat model, mitigations |
| `docs/context/update-system.md` | A/B updates, TUF, deltas, rollouts |
| `docs/context/fleet-and-device-agent.md` | Device enrollment, attestation, policies |
| `docs/plan/phase-*.md` | Detailed phase plans with tasks |
| `docs/db/schema.sql` | Complete Postgres schema |
| `docs/examples/*.yaml` | Pilot specs (kiosk, school-lab, office) |

---

## 🛡 Security

- **Keys never leave `cmd/signer`** — isolated signing service
- **Builds run in ephemeral VMs** — no host contamination
- **Egress allowlist only** — snapshot.debian.org, package cache, object store
- **Content-addressed artifacts** — immutable, SHA-256 keys
- **TUF for updates** — CDN/mirrors untrusted
- **TPM-bound device identity** — mTLS with hardware keys

---

## 🤝 Contributing

1. Read `CLAUDE.md` for invariants and conventions
2. Follow Go 1.25+ style: `context.Context` first, `%w` errors, no panics
3. Run `make lint` before commit
4. PRs need 1 reviewer; security paths need security reviewer
5. Commits: Conventional Commits format

---

## 📄 License

Proprietary — Internal use only.

---

## 🆘 Troubleshooting

| Issue | Solution |
|-------|----------|
| `/dev/kvm` permission denied | `sudo usermod -aG kvm $USER && newgrp kvm` |
| PostgreSQL connection refused | `sudo systemctl start postgresql` |
| mkosi: command not found | `pip3 install "mkosi==24.*"` |
| Go version mismatch | Install Go 1.25+ from golang.org |
| QEMU TCG very slow | Ensure KVM: `ls -la /dev/kvm` |
| libvirtd not starting | `sudo systemctl start libvirtd` |
| Web build fails | `cd web && rm -rf node_modules package-lock.json && npm install` |

---

**Built with ❤️ for organizations that need their own OS.**