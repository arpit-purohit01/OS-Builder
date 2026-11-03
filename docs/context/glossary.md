# Glossary

| Term | Meaning |
|---|---|
| A/B slots | Two copies of the `/usr` partition set; updates write the inactive one, so the running system is never modified |
| AK / EK | TPM Attestation Key (signs quotes) / Endorsement Key (unique per TPM, certified by manufacturer) |
| BIOS (legacy) | Pre-UEFI firmware interface; boots the first sector (MBR/GRUB core) with no signature checks |
| Boot counting | systemd-boot feature: new entry gets N tries; if never marked good, it is skipped next time |
| CAS | Content-addressed storage: objects named by the hash of their contents |
| CNF / SAT | Conjunctive normal form boolean formula / satisfiability solver used to validate option combinations |
| confext / sysext | systemd extension images layering extra files over `/etc` / `/usr` |
| dbx | UEFI variable listing revoked signatures/hashes |
| dm-verity | Kernel device-mapper target verifying each block of a read-only partition against a hash tree |
| DRR | Deficit round robin, a fair-queuing scheduling algorithm |
| erofs | Enhanced read-only file system, compressed, used for `/usr` |
| ESP | EFI System Partition (FAT), where firmware finds boot loaders |
| FastCDC | Fast content-defined chunking algorithm for deduplication and deltas |
| GCRA | Generic cell rate algorithm (rate limiting equivalent to token bucket) |
| HCL | Hardware compatibility list |
| HSM | Hardware security module: stores keys so they can be used but not extracted |
| Initrd | Initial RAM disk: small early userspace that unlocks disks and mounts `/usr` and root |
| KEK / PK / db | UEFI Secure Boot key variables (Key Exchange Key, Platform Key, allowed signature database) |
| Measured boot | Each boot stage hashes the next into TPM PCRs, so the TPM can prove/enforce what booted |
| MOK | Machine Owner Key: extra trusted keys managed by shim |
| mkosi | systemd project tool that builds OS images from distribution packages |
| OSSpec | Our declarative description of an OS |
| PCR | TPM Platform Configuration Register (append-only hash accumulator) |
| PSI | Pressure stall information: kernel metrics of time lost waiting for CPU/memory/IO |
| Rendezvous hashing | Highest-random-weight hashing: deterministic key→node mapping with minimal remapping |
| SBOM | Software bill of materials (SPDX/CycloneDX) |
| shim | Microsoft-signed first-stage loader that lets distributions use their own keys |
| SLSA | Supply-chain Levels for Software Artifacts (provenance framework) |
| systemd-repart | Declarative partitioning tool (build time and first boot) |
| systemd-sysupdate | Declarative A/B update tool writing partitions/files from sources |
| TUF | The Update Framework: signed metadata roles protecting software updates |
| UKI | Unified Kernel Image: kernel+initrd+cmdline+metadata in one signed EFI binary |
| W-TinyLFU | Cache admission/eviction policy combining frequency sketch with LRU segments |
| XBOOTLDR | Extended boot loader partition (Boot Loader Spec) for UKIs when the ESP is small |
| zram | Compressed RAM block device used as fast swap |
