# PortLab — donor ROM -> any device

Standalone Go tool. Does **not** modify `romtool`: it orchestrates the
existing `bin/romtool` binary + `toolkit/bin/*` tools via exec.

## What it does

- Pick donor + target, fingerprint both (arch, SDK/VNDK, Treble/SAR, A/B,
  layout, AVB, kernel, partitions).
- Verdict: `DIRECT_PORT` / `DIRECT_PORT_WITH_FIXES` / `SGSI_RECOMMENDED` /
  `GSI_ONLY` / `INCOMPATIBLE` with score + fixes. This defines whether the
  donor can boot on the target.
- `make` builds `work/rom_root` hybrid: donor `system/product/system_ext`
  + target `vendor/odm/boot`, never donor kernel.
- Refine loop: edit `work/partitions/*` + boot ramdisk with `romtool`,
  then `repack` to flashable zip.
- `to-gsi` fallback: donor system -> standalone GSI/SGSI zip (fastboot
  only, recovery installer aborts on purpose) when direct port cannot boot.
- Editable metadata: `port_project.json` (`name/device/maintainer/version/
  changelog` + custom keys, validated).

## Dynamic / non-dynamic handling (`internal/dyn`)

Layouts detected per side: `dynamic-dat` (op_list + dat/br), `super`
(super.img), `payload` (payload.bin), `nondynamic` (dat/br + mounts),
`raw` (loose .img). Every donor->base combination has a defined path:

| donor -> base | behavior |
|---|---|
| dat/br -> dat family | direct overlay, winner container kept, stale removed |
| dat/br -> raw .img | unpack via romtool -> overlay .img |
| dat/br -> super | unpack via romtool -> rebuild super with lpmake |
| super -> dat family | lpunpack -> img2sdat convert |
| super -> super/raw | lpunpack -> overlay / rebuild |
| payload -> any | payload-dumper -> convert per base |
| raw img -> dat family | img2sdat convert |
| raw img -> super/raw | overlay / rebuild |

Safety rules enforced, not warned:

- op_list `resize` values updated from real image sizes; group-budget
  overflow **refuses** (suggests GSI) — oversized super is a brick vector.
- Rebuilt super must fit the base super budget and re-validate LP magic.
- One container family per partition in output (stale `.br`/`.dat`/`.img`
  removed); `*.patch.dat` never carried over (deltas describe old image).
- super.img validated by LP metadata magic, boot by `ANDROID!`/`VNDRBOOT`,
  system by ext4/erofs/sparse magic, dat by dat+transfer.list pair.

## Anti-brick gates (`make`, `repack`)

Refused always: arch mismatch, missing/corrupt target boot, missing
target vendor, unknown target device, unusable donor system, ZipSlip/
symlink/zip-bomb inputs, updater-script injection via metadata.
`make` also refuses non-bootable verdicts unless `-force` (arch mismatch
refuses even with `-force`). `repack` validates the finished zip (system
+ vendor + boot + updater asserting the target device + dat/transfer
pairs) and **deletes** it on failure. GSI flash scripts check
`fastboot getvar product` and ask for confirmation.

## Build & test

```bash
cd portlab && go vet ./... && go test ./... && go build -o ../bin/portlab ./cmd/portlab
./bin/portlab doctor
```

Tests include a real `make_ext4fs -> lpmake -> lpunpack` super roundtrip
(skipped if toolkit binaries absent) plus refusal tests for every gate.

## Flow (initial port, directly)

```bash
./bin/portlab init -donor <donor.zip|dir> -base <stock.zip|dir> -dir ./ports/myport
./bin/portlab check -project ./ports/myport
./bin/portlab make -project ./ports/myport -mode auto
./bin/portlab refine -project ./ports/myport
./bin/portlab metadata -project ./ports/myport -set maintainer=you -set version=v1
./bin/portlab repack -project ./ports/myport -output ./output/myport.zip
# fallback only if verdict says so:
./bin/portlab to-gsi -project ./ports/myport -sgsi
./bin/portlab menu
```

`refine` unpacks the work tree, spoofs the device fingerprint on the
donor system partitions, applies treble/permissive options, repacks, and
syncs back — fstab/ramdisk stay target-owned. Proven on real ROM data
(init -> make -> refine -> repack -> validated 876MB zip).

## Mismatch audit (`audit [-deep]`)

Every mismatch is graded by worst plausible outcome and the tool
auto-resolves what is safe. `make`/`repack` fail closed on blockers.

| severity | meaning | examples |
|---|---|---|
| HARD_BRICK | may not reach fastboot | donor boot/firmware leakage, super over budget, arch mismatch |
| NO_START | logo/no kernel | corrupt boot.img, invalid super, AVB hard fail |
| NO_BOOT | bootloop/recovery-only | HAL regression (composer/boot/keymaster), sepolicy, fstab, sizes, asserts |
| DEGRADED | boots, features broken | camera/audio/sensors/RIL HAL gaps, VNDK skew |
| INFO | verified OK | marked ✓ (contamination-clean, updater-ok, ...) |

Auto-fixed: asserts, device spoof (`refine`), op_list sizes, container
conversion, slot handling, stale removal, vbmeta at repack.
Detected-only (cannot be papered over): missing HALs, module skew,
SoC-subsystem gaps.

The HAL check is a **delta**, not absolute: donor framework matrices are
compared against the target vendor, then filtered against the donor's own
stock vendor — gaps stock also has are benign (tv/automotive/vr noise
disappears). Matrix versions are alternatives (2.0\|3.0\|4.0: any
satisfies), AIDL linear vs HIDL major.minor, manifest fragments included,
`hardware.boot` graded by A/B-ness. Proven: self-consistent ROM audits
clean; a synthetic 2.1-vs-2.2 composer correctly passes; a truly missing
camera correctly flags.

## Mismatch matrix (all pairings)

| mismatch | handling |
|---|---|
| arch | hard refuse, even with `-force` |
| SDK/VNDK gap | verdict + mode (direct/sgsi/gsi), vendor shims documented |
| dynamic <-> non-dynamic | conversion matrix above, op_list resize + budget check |
| super.img both sides | unpack, slot-aware replace (`_a`/`_b`), lpmake rebuild, budget check |
| A/B slots | target updater kept, slot-aware super, `detectSlots` |
| AVB | vbmeta disabled at repack; GSI ships fastboot-only |
| kernel/fstab/ramdisk | always target-owned, never overlaid |
| device assert | rewritten to validated target codename |
| build fingerprint | `refine` spoofs system family, vendor untouched |
| missing base slot | warning, unflashable partition not shipped |

Layout per project: `donor/rom_root`, `base/rom_root`, `work/rom_root`,
`output/`, `port_project.json`, `port_report.json`.
