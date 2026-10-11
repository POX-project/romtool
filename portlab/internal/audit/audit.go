// Package audit finds every mismatch that can stop a port from booting,
// grades it by worst-case outcome, and states what the tool already fixed.
//
// Severity taxonomy (worst plausible outcome, wording qualified in output):
//
//	HardBrick — flashing risks a device that won't reach fastboot
//	            (wrong bootloader/modem fused, super over budget).
//	NoStart   — powers on, never reaches kernel/bootanimation
//	            (bad boot.img, dtbo mismatch, AVB hard fail).
//	NoBoot    — bootloop / recovery-only (HAL, sepolicy, fstab, sizes).
//	Degraded  — boots with broken features (camera/audio/sensors/RIL).
//	Info      — context, no action needed.
//
// The tool auto-resolves what is safe (asserts, device spoof, op_list
// sizes, container conversion, slot handling, vbmeta at repack) and
// reports the rest with concrete next steps. Missing HALs, kernel-module
// skew and SoC-subsystem gaps are detected, never papered over.
package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"portlab/internal/dyn"
	"portlab/internal/project"
	"portlab/internal/util"
)

// Severity is the worst plausible outcome of a finding.
type Severity string

const (
	HardBrick Severity = "HARD_BRICK"
	NoStart   Severity = "NO_START"
	NoBoot    Severity = "NO_BOOT"
	Degraded  Severity = "DEGRADED"
	Info      Severity = "INFO"
)

// Finding is one mismatch with its fix state.
type Finding struct {
	ID        string   `json:"id"`
	Severity  Severity `json:"severity"`
	Category  string   `json:"category"`
	Detail    string   `json:"detail"`
	AutoFixed bool     `json:"auto_fixed"`
	Next      string   `json:"next"`
}

// Report is the full mismatch audit.
type Report struct {
	Findings []Finding `json:"findings"`
}

// Counts returns findings per severity.
func (r *Report) Counts() map[Severity]int {
	m := map[Severity]int{}
	for _, f := range r.Findings {
		m[f.Severity]++
	}
	return m
}

// Blockers returns findings that must stop a flash.
func (r *Report) Blockers() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == HardBrick || f.Severity == NoStart || f.Severity == NoBoot {
			out = append(out, f)
		}
	}
	return out
}

// add appends a finding.
func (r *Report) add(id string, sev Severity, cat, detail string, fixed bool, next string) {
	r.Findings = append(r.Findings, Finding{
		ID: id, Severity: sev, Category: cat, Detail: detail, AutoFixed: fixed, Next: next,
	})
}

// bootFamily marks files that touch the bootloader/kernel chain.
func bootFamily(rel string) bool {
	base := filepath.Base(rel)
	for _, p := range []string{"boot.img", "init_boot.img", "vendor_boot.img", "dtbo", "vbmeta", "modem", "tz", "bootloader", "abl", "xbl", "rpm", "hyp", "keymaster", "cmnlib"} {
		if base == p+".img" || base == p || strings.HasPrefix(base, p+"_") || strings.HasPrefix(base, p+".") {
			return true
		}
	}
	return false
}

// Audit runs file-level checks on work/rom_root (or base if no work yet).
// No unpacking, always fast. Defense in depth: the merge is constructed to
// satisfy these, so any failure here means contamination or corruption.
func Audit(p *project.Project) *Report {
	r := &Report{}
	root := filepath.Join(p.WorkDir, "rom_root")
	if !util.FileExists(root) {
		root = p.BaseRoot
		r.add("no-work", Info, "state", "no work/ yet — auditing base (run make first)", false,
			"portlab make -project <dir>")
	}
	overlaid := overlaidSet(p)
	contaminationAudit(r, p, root, overlaid)
	bootChainAudit(r, p, root)
	layoutAudit(r, p, root)
	updaterAudit(r, p, root)
	socAudit(r, p)
	return r
}

// overlaidSet reads port_report.json for donor-overlaid partitions.
func overlaidSet(p *project.Project) map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(p.Dir, "port_report.json"))
	if err != nil {
		return out
	}
	var rep map[string]any
	if err := json.Unmarshal(data, &rep); err != nil {
		return out
	}
	if ov, ok := rep["overlaid"].([]any); ok {
		for _, v := range ov {
			if s, ok := v.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

// contaminationAudit asserts every work file is either byte-identical to
// base, a donor overlay for an allowlisted partition, or tool-generated.
// Anything else is unexpected donor leakage — graded by file family.
func contaminationAudit(r *Report, p *project.Project, root string, overlaid map[string]bool) {
	gen := map[string]bool{
		"dynamic_partitions_op_list": true, "PORT_NOTES.txt": true,
		"README-SGSI.txt": true,
	}
	updater := filepath.Join("META-INF", "com", "google", "android", "updater-script")
	var diffs, linked int
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		slash := filepath.ToSlash(rel)
		basePath := filepath.Join(p.BaseRoot, rel)
		// Same inode across the project boundary (old hardlinked trees):
		// an in-place edit would corrupt the pristine base.
		if bi, err := os.Stat(basePath); err == nil && os.SameFile(info, bi) {
			linked++
		}
		if gen[slash] || gen[filepath.Base(rel)] || slash == filepath.ToSlash(updater) {
			return nil
		}
		// Boot-chain files get byte-exact compare (small, critical);
		// bulk images compare by size (fast on GB ROMs).
		full := bootFamily(rel)
		if filesEqualQuick(path, basePath, full) {
			return nil
		}
		// Transfer-list total corrections (E1001 guard) change only the
		// derived total line — same commands, not contamination.
		if strings.HasSuffix(filepath.Base(rel), ".transfer.list") && transferSameCommands(path, basePath) {
			return nil
		}
		// Allowlisted donor overlay?
		if part, ok := overlayFile(slash); ok && overlaid[part] {
			donorPath := filepath.Join(p.DonorRoot, filepath.Base(rel))
			if filesEqualQuick(path, donorPath, full) {
				return nil
			}
			// Converted files (img->dat) match neither — accepted if the
			// partition is allowlisted and the file validates below.
			if validOverlayFile(path) {
				return nil
			}
		}
		// Super rebuild is by design different — validated separately.
		if filepath.Base(rel) == "super.img" && dyn.Detect(p.BaseRoot).Layout == dyn.LayoutSuper {
			return nil
		}
		diffs++
		if bootFamily(rel) {
			r.add("contam/"+slash, HardBrick, "contamination",
				fmt.Sprintf("%s differs from base and is not an allowlisted overlay — bootloader/kernel-chain file, flashing risks hard brick", slash),
				false, "restore from base, then re-run make; never flash donor boot/firmware")
		} else {
			r.add("contam/"+slash, NoBoot, "contamination",
				fmt.Sprintf("%s differs from base and is not an allowlisted overlay — possible donor leakage, likely bootloop", slash),
				false, "verify the file is intentional; else restore from base")
		}
		return nil
	})
	if diffs == 0 {
		r.add("contam-clean", Info, "contamination", "no unexpected donor leakage: non-overlay files match base", true, "")
	}
	if linked > 0 {
		r.add("hardlink-boundary", Degraded, "contamination",
			fmt.Sprintf("%d work file(s) share inodes with base (old hardlinked tree) — in-place edits would corrupt the pristine source", linked),
			false, "re-run make to materialize real copies")
	}
}

// overlayFile maps a container filename to its partition, if allowlisted.
func overlayFile(slash string) (string, bool) {
	base := filepath.Base(slash)
	for _, part := range []string{"system", "system_ext", "product"} {
		for _, suf := range []string{".new.dat.br", ".new.dat", ".transfer.list", ".patch.dat", ".img"} {
			if base == part+suf {
				return part, true
			}
		}
	}
	return "", false
}

// validOverlayFile checks a converted overlay file is sane.
func validOverlayFile(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".img") {
		_, err := dyn.ValidateImage(path, 1<<20)
		return err == nil
	}
	if strings.HasSuffix(base, ".new.dat") || strings.HasSuffix(base, ".new.dat.br") {
		st, err := os.Stat(path)
		return err == nil && st.Size() > 0
	}
	if strings.HasSuffix(base, ".transfer.list") {
		st, err := os.Stat(path)
		return err == nil && st.Size() > 0
	}
	return false
}

// bootChainAudit validates boot/dtbo/vbmeta presence and integrity.
func bootChainAudit(r *Report, p *project.Project, root string) {
	boot := ""
	for _, n := range []string{"boot.img", "init_boot.img"} {
		if c := filepath.Join(root, n); util.FileExists(c) {
			boot = c
			break
		}
	}
	if boot == "" {
		r.add("boot-missing", NoStart, "boot", "no boot.img — device cannot start a kernel", false,
			"use a full stock firmware as base")
	} else if err := dyn.ValidateBootImg(boot); err != nil {
		r.add("boot-corrupt", NoStart, "boot", "boot.img invalid: "+err.Error(), false,
			"restore target boot.img, never flash donor boot")
	} else {
		r.add("boot-ok", Info, "boot", "target boot.img valid, donor kernel excluded", true,
			"never replace with donor boot.img")
	}
	if util.FileExists(filepath.Join(root, "vbmeta.img")) {
		flags, err := vbmetaFlags(filepath.Join(root, "vbmeta.img"))
		switch {
		case err != nil:
			r.add("vbmeta-unreadable", NoBoot, "avb", "vbmeta.img unreadable: "+err.Error(), false,
				"restore target vbmeta.img")
		case flags == 3:
			r.add("vbmeta-disabled", Info, "avb", "vbmeta verification disabled (flags 3)", true, "")
		default:
			r.add("vbmeta-enabled", NoBoot, "avb",
				fmt.Sprintf("vbmeta flags=%d (verification on) — modified partitions will fail verification", flags),
				false, "repack disables it (flags 3); do not flash before repack")
		}
	} else {
		r.add("vbmeta-absent", Degraded, "avb", "no vbmeta.img — bootloader skips verification, boots insecurely", false,
			"prefer a base that ships vbmeta, or accept the warning")
	}
}

// layoutAudit checks op_list/super coherence and budgets.
func layoutAudit(r *Report, p *project.Project, root string) {
	info := dyn.Detect(root)
	r.add("layout", Info, "layout",
		fmt.Sprintf("output layout %s (donor %s -> base %s)", info.Layout, p.Donor.Layout, p.Base.Layout),
		true, "")
	switch info.Layout {
	case dyn.LayoutDynamicDat:
		op, err := dyn.ParseOpList(filepath.Join(root, "dynamic_partitions_op_list"))
		if err != nil {
			r.add("oplist-unreadable", NoBoot, "dynamic", "op_list unreadable: "+err.Error(), false,
				"re-run make to regenerate")
			return
		}
		for gname, use := range op.GroupUsage() {
			switch {
			case use[1] > 0 && use[0] > use[1]:
				r.add("group-overflow/"+gname, HardBrick, "dynamic",
					fmt.Sprintf("group %s over budget %d > %d — super would flash truncated", gname, use[0], use[1]),
					false, "shrink donor system or use to-gsi fallback")
			case use[1] > 0 && use[0] > use[1]*95/100:
				r.add("group-tight/"+gname, NoBoot, "dynamic",
					fmt.Sprintf("group %s at %d%% of budget — flash may fail on slot retry", gname, use[0]*100/use[1]),
					false, "trim system or use GSI")
			default:
				r.add("group-ok/"+gname, Info, "dynamic",
					fmt.Sprintf("group %s fits (%d/%d)", gname, use[0], use[1]), true, "")
			}
		}
	case dyn.LayoutSuper:
		sp := filepath.Join(root, "super.img")
		if err := dyn.ValidateSuper(sp, 1<<20); err != nil {
			r.add("super-invalid", NoStart, "dynamic", "super.img invalid: "+err.Error(), false,
				"re-run make to rebuild super")
		} else if budget := dyn.Detect(p.BaseRoot).SuperSize; budget > 0 {
			if st, _ := os.Stat(sp); st != nil && st.Size() > budget {
				r.add("super-oversize", HardBrick, "dynamic", "rebuilt super exceeds base super budget", false,
					"use to-gsi fallback")
			} else {
				r.add("super-ok", Info, "dynamic", "super.img valid and within budget", true, "")
			}
		} else {
			r.add("super-ok", Info, "dynamic", "super.img valid", true, "")
		}
	}

	// Non-dynamic partition capacity check: ensure repacked transfer.list does not overflow target partition.
	if info.Layout == dyn.LayoutNonDynamic || info.Layout == dyn.LayoutRaw {
		for _, part := range []string{"system", "vendor", "product", "system_ext", "odm"} {
			workTL := filepath.Join(root, part+".transfer.list")
			baseTL := filepath.Join(p.BaseRoot, part+".transfer.list")
			if util.FileExists(workTL) && util.FileExists(baseTL) {
				_, baseErase, baseCmds, errB := dyn.ParseTransferList(baseTL)
				_, workErase, workCmds, errW := dyn.ParseTransferList(workTL)
				if errB == nil && errW == nil {
					baseMax := dyn.MaxRangeEnd(baseCmds)
					if baseErase > baseMax {
						baseMax = baseErase
					}
					workMax := dyn.MaxRangeEnd(workCmds)
					if workErase > workMax {
						workMax = workErase
					}
					if baseMax > 0 && workMax > baseMax {
						r.add("partition-overflow/"+part, HardBrick, "capacity",
							fmt.Sprintf("partition %s addresses %d blocks, exceeding target partition capacity %d blocks (%d bytes) — recovery will abort with E1001",
								part, workMax, baseMax, baseMax*4096),
							false, "repack with target partition size or debloat")
					} else if baseMax > 0 {
						r.add("partition-budget-ok/"+part, Info, "capacity",
							fmt.Sprintf("partition %s fits within target capacity (%d <= %d blocks)", part, workMax, baseMax),
							true, "")
					}
				}
			}
		}
	}
	// Vendor presence (loose files here; containerized covered by make gates).
	for _, n := range []string{"vendor", "odm"} {
		found := false
		for _, suf := range []string{".new.dat.br", ".new.dat", ".img"} {
			if util.FileExists(filepath.Join(root, n+suf)) {
				found = true
				break
			}
		}
		if found {
			r.add(n+"-ok", Info, "vendor", "target "+n+" present (HALs preserved)", true, "")
		}
	}
	if !util.FileExists(filepath.Join(root, "vendor.new.dat.br")) &&
		!util.FileExists(filepath.Join(root, "vendor.new.dat")) &&
		!util.FileExists(filepath.Join(root, "vendor.img")) &&
		!util.FileExists(filepath.Join(root, "odm.new.dat.br")) &&
		!util.FileExists(filepath.Join(root, "odm.img")) &&
		info.Layout != dyn.LayoutSuper && info.Layout != dyn.LayoutPayload {
		r.add("vendor-missing", NoBoot, "vendor", "no target vendor/odm — HALs missing, guaranteed bootloop", false,
			"use a full stock firmware as base")
	}
}

// updaterAudit verifies the device assert targets the right phone.
func updaterAudit(r *Report, p *project.Project, root string) {
	updater := filepath.Join(root, "META-INF", "com", "google", "android", "updater-script")
	data, err := os.ReadFile(updater)
	if err != nil {
		// Fastboot-flow layouts (payload/super) ship no recovery
		// installer by design — degraded, not broken.
		sev := NoBoot
		next := "add META-INF from the stock base"
		if info := dyn.Detect(root); info.Layout == dyn.LayoutPayload || info.Layout == dyn.LayoutSuper {
			sev = Degraded
			next = "fastboot flow: flash super/images directly, see PORT_NOTES"
		}
		r.add("updater-missing", sev, "updater", "no updater-script — not recovery-flashable", false, next)
		return
	}
	// Device resolution mirrors the merge (meta -> base -> updater devices).
	device := ""
	for _, cand := range []string{p.Meta.Device, p.Base.Device} {
		if cand != "" && cand != "unknown" {
			device = cand
			break
		}
	}
	if device == "" && len(p.Base.UpdaterDevices) > 0 {
		device = p.Base.UpdaterDevices[0]
	}
	if device != "" && device != "unknown" && strings.Contains(string(data), device) {
		r.add("updater-ok", Info, "updater", "assert targets "+device+" (wrong-device flash blocked)", true, "")
	} else {
		r.add("updater-assert", NoBoot, "updater", "updater-script does not assert the target device", false,
			"portlab metadata -set device=<codename>, then re-run make")
	}
	// Donor assert leftovers would let the zip claim the wrong phone.
	if p.Donor.Device != "" && p.Donor.Device != "unknown" && p.Donor.Device != device &&
		strings.Contains(string(data), `"`+p.Donor.Device+`"`) {
		r.add("updater-donor-leak", NoBoot, "updater", "updater-script still asserts donor device "+p.Donor.Device, false,
			"re-run make to rewrite asserts")
	}
}

// socAudit compares SoCs and lists what automation covers vs manual testing.
func socAudit(r *Report, p *project.Project) {
	dPlat, bPlat := p.Donor.Platform, p.Base.Platform
	dArch, bArch := p.Donor.Arch, p.Base.Arch
	switch {
	case dPlat != "" && bPlat != "" && strings.EqualFold(dPlat, bPlat):
		r.add("soc-same", Info, "soc",
			fmt.Sprintf("same platform %s — kernel/HAL families match, highest boot chance", bPlat), true,
			"remaining risks: sizes, asserts, props (all automated)")
	case dArch != "" && bArch != "" && !strings.EqualFold(dArch, bArch) && dArch != "unknown" && bArch != "unknown":
		r.add("soc-arch", HardBrick, "soc",
			fmt.Sprintf("CPU arch differs (%s vs %s) — binaries cannot execute", dArch, bArch), false,
			"pick a donor with matching ABI; no fallback exists")
	default:
		r.add("soc-cross", Degraded, "soc",
			fmt.Sprintf("platform donor=%s target=%s — boots via target vendor, peripherals need testing", orUnknown(dPlat), orUnknown(bPlat)),
			false, "after flashing, test camera/audio/sensors/RIL/fingerprint/NFC; collect logcat -b crash + tombstones")
		for _, s := range []string{
			"display/composer (boot-critical): if bootloops at animation, suspect graphics HAL — logcat -b all | grep -i composer",
			"RIL/modem: stays on target vendor — test calls/data, check getprop gsm.sim.state",
			"camera/audio/sensors: vendor-owned; failures are degraded-mode, check dumpsys media.camera/sensorservice",
		} {
			r.add("soc-check", Degraded, "soc", s, false, "see crash-analyzer agent on failure")
		}
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// transferSameCommands reports whether two transfer.lists differ at most
// in the derived total line (line 2). Command bodies must match exactly.
func transferSameCommands(a, b string) bool {
	da, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	db, err := os.ReadFile(b)
	if err != nil {
		return false
	}
	la, lb := strings.Split(string(da), "\n"), strings.Split(string(db), "\n")
	if len(la) != len(lb) {
		return false
	}
	for i := range la {
		if i == 1 {
			continue // derived total, allowed to differ (E1001 correction)
		}
		if la[i] != lb[i] {
			return false
		}
	}
	return true
}

// filesEqualQuick compares bytes when full, else size only (fast on GB
// images; boot-chain callers always use full).
func filesEqualQuick(a, b string, full bool) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	if sa.Size() != sb.Size() {
		if filepath.Base(a) == "boot.img" && (sa.Size() == sb.Size()+16 || sb.Size() == sa.Size()+16) {
			da, errA := os.ReadFile(a)
			db, errB := os.ReadFile(b)
			if errA == nil && errB == nil {
				return bytes.Equal(bytes.TrimSuffix(da, []byte("SEANDROIDENFORCE")), bytes.TrimSuffix(db, []byte("SEANDROIDENFORCE")))
			}
		}
		return false
	}
	if !full {
		return true
	}
	da, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	db, err := os.ReadFile(b)
	if err != nil {
		return false
	}
	return bytes.Equal(bytes.TrimSuffix(da, []byte("SEANDROIDENFORCE")), bytes.TrimSuffix(db, []byte("SEANDROIDENFORCE")))
}

// vbmetaFlags reads AVB flags (uint32 BE at offset 120).
func vbmetaFlags(path string) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	hdr := make([]byte, 124)
	n, err := f.Read(hdr)
	if err != nil || n < 124 {
		return 0, fmt.Errorf("too small for AVB header")
	}
	if string(hdr[:4]) != "AVB0" {
		return 0, fmt.Errorf("bad AVB magic")
	}
	return uint32(hdr[120])<<24 | uint32(hdr[121])<<16 | uint32(hdr[122])<<8 | uint32(hdr[123]), nil
}

// SortedSev orders severities worst-first for display.
func SortedSev() []Severity {
	return []Severity{HardBrick, NoStart, NoBoot, Degraded, Info}
}

// BySeverity groups findings worst-first.
func (r *Report) BySeverity() map[Severity][]Finding {
	out := map[Severity][]Finding{}
	for _, f := range r.Findings {
		out[f.Severity] = append(out[f.Severity], f)
	}
	for _, v := range out {
		sort.Slice(v, func(i, j int) bool { return v[i].ID < v[j].ID })
	}
	return out
}
