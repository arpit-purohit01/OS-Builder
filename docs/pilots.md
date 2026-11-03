# Pilot customers and hardware matrix (Phase 0, P0-01)

Fill one section per pilot after the discovery calls (2 calls each). This is
customer-facing work; the template below defines what must be captured.

## <Customer name>
| Topic | Answer |
|---|---|
| Use case | kiosk / thin client / office desktop / lab / server |
| Fleet size and sites | |
| Device models (DMI vendor/product) | |
| CPU / GPU / Wi-Fi chips (`lspci -nn`, `lsusb`) | |
| RAM / disk | |
| Firmware: UEFI version, Secure Boot state, setup-mode access, TPM 2.0 present | |
| Windows co-existence (dual boot, BitLocker) | |
| Network constraints (proxy, offline sites, allowed hosts) | |
| Current imaging process and pain points | |
| Apps (catalog / Flatpak / web / custom .deb) | |
| Identity provider (Entra ID, Google, Okta, LDAP) | |

## Hardware matrix (all pilots)
| Model | Count | CPU | GPU | Wi-Fi | TPM | SB keys enrollable | Tested image | Result |
|---|---|---|---|---|---|---|---|---|
| QEMU q35 + OVMF + swtpm (reference) | — | x86-64 | virtio | virtio-net | 2.0 (swtpm) | yes (setup mode) | ci-server 1.0.0 | Phase 0 acceptance |
