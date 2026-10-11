// Package engine executes the port: hybrid merge, GSI/SGSI fallback,
// boot-readiness loop, and repack. It orchestrates the existing romtool
// binary and toolkit binaries via exec — it never modifies romtool source.
package engine

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"portlab/internal/audit"
	"portlab/internal/compat"
	"portlab/internal/dyn"
	"portlab/internal/project"
	"portlab/internal/util"
)

type Options struct {
	Mode       string // direct|sgsi|gsi|auto
	Permissive bool
	AvbDisable bool
	Force      bool // bypass CanBoot refusal (never bypasses arch mismatch or missing boot)
}

type BootCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func DonorPartitionsForMode(mode string) []string {
	if mode == "gsi" {
		return []string{"system"}
	}
	return []string{"system", "system_ext", "product"}
}

// Make builds work/rom_root as donor-system + target-vendor hybrid,
// converting between dynamic / non-dynamic layouts as needed.
//
// Hard gates (never bypassed, not even with Force — violating them
// produces unbootable or bricking output):
//
//	arch mismatch, missing/corrupt target boot, missing target vendor,
//	unknown target device, no usable donor system source.
func Make(p *project.Project, opts Options) error {
	if err := validateWorkDir(p); err != nil {
		return err
	}
	rep := compat.Check(&p.Donor, &p.Base)
	mode := opts.Mode
	if mode == "" || mode == "auto" {
		mode = rep.SuggestedMode
	}
	if mode == "none" {
		return fmt.Errorf("incompatible (%s, score %d) — run to-gsi fallback instead", rep.Verdict, rep.Score)
	}
	if mode != "direct" && mode != "sgsi" && mode != "gsi" {
		mode = "direct"
	}
	// Arch mismatch can never boot — no mode or flag bypasses this.
	if knownBoth(p.Donor.Arch, p.Base.Arch) && !archOK(p.Donor.Arch, p.Base.Arch) {
		return fmt.Errorf("ARCH MISMATCH donor %s vs target %s — refusing (no image can fix this)",
			p.Donor.Arch, p.Base.Arch)
	}
	if !rep.CanBoot && !opts.Force {
		return fmt.Errorf("%s (score %d) — refusing without -force; use to-gsi fallback or re-check",
			rep.Verdict, rep.Score)
	}
	// Target boot must exist and be a real boot image (wrong kernel = brick).
	// Super/payload bases may carry it inside their container.
	stageDir := filepath.Join(p.Dir, ".stage")
	_ = os.MkdirAll(stageDir, 0755)
	bootPath := bootFile(p.BaseRoot)
	if bootPath == "" && dyn.Detect(p.BaseRoot).Layout == dyn.LayoutPayload {
		bootPath = payloadBoot(p.BaseRoot, stageDir)
	}
	if bootPath == "" {
		return fmt.Errorf("no boot.img/init_boot.img in target — refusing (unbootable without target kernel)")
	}
	if err := dyn.ValidateBootImg(bootPath); err != nil {
		return fmt.Errorf("target boot corrupt: %w — refusing", err)
	}
	// Target vendor/odm must exist (a port without HALs is a bootloop).
	if !vendorPresent(p.BaseRoot, stageDir) {
		return fmt.Errorf("no target vendor/odm — refusing (use full stock firmware as base)")
	}
	// Target device must be known — a "target_device" assert zip always aborts.
	target, err := resolveTargetDevice(p)
	if err != nil {
		return err
	}
	donorInfo := dyn.Detect(p.DonorRoot)
	baseInfo := dyn.Detect(p.BaseRoot)

	if err := preflight(p.Dir, p.BaseRoot, 2.0, "make (base clone + overlays)"); err != nil {
		return err
	}
	workRoot := filepath.Join(p.WorkDir, "rom_root")
	_ = os.RemoveAll(p.WorkDir)
	_ = os.MkdirAll(workRoot, 0755)
	// 1. Clone base (target) — flashing logic must match target device.
	if err := util.CopyDirNoLink(p.BaseRoot, workRoot); err != nil {
		return fmt.Errorf("clone base failed: %w", err)
	}
	// 1b. Materialize containerized base boot/vendor (payload/super probe).
	if err := materializeBase(workRoot, stageDir); err != nil {
		return err
	}
	// 2. Overlay donor partitions, converting families as needed.
	want := DonorPartitionsForMode(mode)
	overlaid, converted, warnings, err := overlayDonor(p, donorInfo, baseInfo, workRoot, stageDir, want)
	if err != nil {
		return err
	}
	// 3. Target device asserts (validated — no script injection possible).
	if util.FileExists(filepath.Join(workRoot, "META-INF", "com", "google", "android", "updater-script")) {
		if err := RewriteUpdater(filepath.Join(workRoot, "META-INF", "com", "google", "android", "updater-script"), target); err != nil {
			return err
		}
	} else {
		warnings = append(warnings, "no updater-script in base — output needs manual META-INF")
	}
	// 4. AVB note (actual flag patch happens in romtool repack stage).
	if opts.AvbDisable && util.FileExists(filepath.Join(workRoot, "vbmeta.img")) {
		warnings = append(warnings, "vbmeta present — repack stage disables verification (flags 3)")
	}
	// 5. Best-effort build.prop patch if trees already unpacked nearby.
	notes := refineUnpacked(p, mode, opts)
	report := map[string]any{
		"mode": mode, "score": rep.Score, "verdict": string(rep.Verdict),
		"donor_device": p.Donor.Device, "base_device": p.Base.Device,
		"donor_layout": string(donorInfo.Layout), "base_layout": string(baseInfo.Layout),
		"overlaid": overlaid, "converted": converted, "warnings": warnings, "refine_notes": notes,
		"kept_from_base":   []string{"vendor", "odm", "boot", "vendor_boot", "dtbo", "vbmeta", "firmware", "META-INF"},
		"taken_from_donor": want,
		"created_at":       time.Now().Format(time.RFC3339),
		"next":             "Refine work/partitions + boot ramdisk (via romtool), then: portlab repack -project <dir>",
	}
	if mode == "sgsi" {
		report["sgsi_notes"] = "Phh-Treble style: donor system on target vendor; shim RIL/audio/camera."
		_ = util.WriteFile(filepath.Join(p.Dir, "output", "README-SGSI.txt"),
			fmt.Sprintf("SGSI for %s from %s (score %d %s)\nAdd vendor shims for RIL/audio/camera.\n", p.Base.Device, p.Donor.Device, rep.Score, rep.Verdict))
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	_ = util.WriteFile(filepath.Join(p.Dir, "port_report.json"), string(data))
	_ = util.WriteFile(filepath.Join(p.WorkDir, "PORT_NOTES.txt"), PortNotes(p, mode, overlaid, warnings))
	// Fail closed: self-audit the produced tree. vbmeta-enabled is
	// expected here (repack disables it), so it never blocks make.
	arep := audit.Audit(p)
	if blockers := hardBlockers(arep, map[string]bool{"vbmeta-enabled": true}); len(blockers) > 0 {
		_ = os.RemoveAll(p.WorkDir)
		return fmt.Errorf("self-audit found %d blocker(s), output removed — first: [%s] %s: %s",
			len(blockers), blockers[0].Severity, blockers[0].ID, blockers[0].Detail)
	}
	p.Mode, p.Verdict, p.Score, p.LastReport = mode, string(rep.Verdict), rep.Score, filepath.Join(p.Dir, "port_report.json")
	p.Mode, p.Verdict, p.Score, p.LastReport = mode, string(rep.Verdict), rep.Score, filepath.Join(p.Dir, "port_report.json")
	if p.Meta.Device == "" || p.Meta.Device == "unknown" {
		p.Meta.Device = target
	}
	return p.Save()
}

// overlayDonor stages donor partitions and converts them into the base
// family's containers. Returns overlaid + converted lists.
func overlayDonor(p *project.Project, donorInfo, baseInfo *dyn.Info, workRoot, stageDir string, want []string) ([]string, []string, []string, error) {
	var warnings []string
	if err := ensurePayloadReady(p.DonorRoot, want); err != nil {
		warnings = append(warnings, "payload: "+err.Error())
	}
	switch baseInfo.Layout {
	case dyn.LayoutSuper:
		return overlaySuperBase(p, donorInfo, baseInfo, workRoot, stageDir, want, warnings)
	case dyn.LayoutDynamicDat, dyn.LayoutNonDynamic:
		return overlayDatBase(p, donorInfo, baseInfo, workRoot, stageDir, want, warnings)
	case dyn.LayoutRaw:
		return overlayRawBase(p, donorInfo, workRoot, stageDir, want, warnings)
	default:
		return nil, nil, warnings, fmt.Errorf("base layout %q not recognized — refusing (overlays would be ambiguous)", baseInfo.Layout)
	}
}

// overlayDatBase handles dynamic-dat and nondynamic bases (same dat family).
func overlayDatBase(p *project.Project, donorInfo, baseInfo *dyn.Info, workRoot, stageDir string, want, warnings []string) ([]string, []string, []string, error) {
	var overlaid, converted []string
	stagedSizes := map[string]int64{}
	for _, name := range want {
		if dyn.Format(p.DonorRoot, name) == dyn.SrcMissing {
			warnings = append(warnings, "donor has no "+name+" (skipped)")
			continue
		}
		if (baseInfo.Layout == dyn.LayoutNonDynamic || baseInfo.Layout == dyn.LayoutRaw) &&
			dyn.Format(p.BaseRoot, name) == dyn.SrcMissing {
			warnings = append(warnings, fmt.Sprintf("base has no %s slot — donor %s ships unflashed on this layout (drop it or repartition)", name, name))
			continue
		}
		src := dyn.Format(p.DonorRoot, name)
		switch src {
		case dyn.SrcDat, dyn.SrcDatBr:
			if err := dyn.ValidateDatPair(p.DonorRoot, name); err != nil {
				return nil, nil, warnings, fmt.Errorf("donor %s: %w", name, err)
			}
			wonBr := false
			for _, f := range FindPartitionFiles(p.DonorRoot, name) {
				if strings.HasSuffix(f, ".new.dat.br") {
					wonBr = true
				}
				if err := util.CopyFile(f, filepath.Join(workRoot, filepath.Base(f))); err != nil {
					warnings = append(warnings, filepath.Base(f)+": "+err.Error())
				}
			}
			if wonBr {
				removeStale(workRoot, name, keepBr)
			} else {
				removeStale(workRoot, name, keepDat)
			}
			overlaid = append(overlaid, name)
			// Free budget check: donor's own op_list knows its raw size.
			if sz, ok := donorInfo.Sizes[name]; ok && sz > 0 {
				stagedSizes[name] = sz
			}
		case dyn.SrcImg, dyn.SrcSuperLogical, dyn.SrcPayload:
			raw, err := stageDonorRaw(p.DonorRoot, stageDir, name)
			if err != nil {
				warnings = append(warnings, name+": "+err.Error()+" (skipped)")
				continue
			}
			if err := rawToDat(raw, workRoot, name); err != nil {
				return nil, nil, warnings, fmt.Errorf("convert %s img->dat: %w", name, err)
			}
			removeStale(workRoot, name, keepDat)
			overlaid = append(overlaid, name)
			converted = append(converted, name+":img->dat")
		default:
			warnings = append(warnings, "donor has no "+name+" (skipped)")
		}
	}
	if len(overlaid) == 0 {
		return nil, nil, warnings, fmt.Errorf("no donor system source usable — refusing base-only output mislabeled as port")
	}
	// Refresh op_list sizes where the raw size is known; validate budget.
	if baseInfo.Layout == dyn.LayoutDynamicDat {
		if err := refreshOpList(workRoot, stageDir, overlaid, stagedSizes); err != nil {
			return nil, nil, warnings, err
		}
	}
	_ = donorInfo
	return overlaid, converted, warnings, nil
}

// overlayRawBase handles loose-.img bases.
func overlayRawBase(p *project.Project, donorInfo *dyn.Info, workRoot, stageDir string, want, warnings []string) ([]string, []string, []string, error) {
	var overlaid, converted []string
	for _, name := range want {
		// Raw/fastboot layouts flash explicit files, so a donor image for
		// a slot the base lacks is still usable (unlike updater-driven
		// dat layouts, where it would ship unflashed).
		freshSlot := dyn.Format(p.BaseRoot, name) == dyn.SrcMissing
		if freshSlot {
			warnings = append(warnings, fmt.Sprintf("base has no %s.img — adding donor image fresh (verify flashing covers it)", name))
		}
		src := dyn.Format(p.DonorRoot, name)
		switch src {
		case dyn.SrcImg:
			raw, err := stageDonorRaw(p.DonorRoot, stageDir, name)
			if err != nil {
				warnings = append(warnings, name+": "+err.Error())
				continue
			}
			if err := util.CopyFile(raw, filepath.Join(workRoot, name+".img")); err != nil {
				warnings = append(warnings, name+": "+err.Error())
				continue
			}
			removeStale(workRoot, name, keepImg)
			overlaid = append(overlaid, name)
		case dyn.SrcDat, dyn.SrcDatBr, dyn.SrcSuperLogical, dyn.SrcPayload:
			raw, err := stageDonorRaw(p.DonorRoot, stageDir, name)
			if err != nil {
				warnings = append(warnings, name+": "+err.Error()+" (skipped)")
				continue
			}
			if err := util.CopyFile(raw, filepath.Join(workRoot, name+".img")); err != nil {
				warnings = append(warnings, name+": "+err.Error())
				continue
			}
			removeStale(workRoot, name, keepImg)
			overlaid = append(overlaid, name)
			converted = append(converted, name+"->img")
		default:
			warnings = append(warnings, "donor has no "+name+" (skipped)")
		}
	}
	if len(overlaid) == 0 {
		return nil, nil, warnings, fmt.Errorf("no donor system source usable — refusing base-only output")
	}
	_ = donorInfo
	return overlaid, converted, warnings, nil
}

// overlaySuperBase rebuilds the target super.img with donor logicals.
func overlaySuperBase(p *project.Project, donorInfo, baseInfo *dyn.Info, workRoot, stageDir string, want, warnings []string) ([]string, []string, []string, error) {
	// Unpack the CLONED base super (work copy, never the pristine base).
	baseSuper := filepath.Join(workRoot, "super.img")
	logicalDir := filepath.Join(stageDir, "base_super")
	_ = os.RemoveAll(logicalDir)
	if err := unpackSuperFile(baseSuper, logicalDir); err != nil {
		return nil, nil, warnings, fmt.Errorf("base super unpack: %w", err)
	}
	var overlaid, converted []string
	for _, name := range want {
		raw, err := stageDonorRaw(p.DonorRoot, stageDir, name)
		if err != nil {
			warnings = append(warnings, name+": "+err.Error()+" (kept target logical)")
			continue
		}
		// Slot-suffixed logicals (system_a/system_b): replace every
		// variant — writing plain system.img would ADD a partition
		// instead of replacing, producing a corrupt super.
		targets := slottedVariants(logicalDir, name)
		if len(targets) == 0 {
			targets = []string{name + ".img"}
			warnings = append(warnings, name+": no target logical, adding fresh (verify base super had this partition)")
		}
		ok := false
		for _, t := range targets {
			if err := util.CopyFile(raw, filepath.Join(logicalDir, t)); err != nil {
				warnings = append(warnings, name+": "+err.Error())
			} else {
				ok = true
			}
		}
		if !ok {
			continue
		}
		overlaid = append(overlaid, name)
		converted = append(converted, name+"->super")
	}
	if len(overlaid) == 0 {
		return nil, nil, warnings, fmt.Errorf("no donor system source usable — refusing to rebuild super unchanged")
	}
	budget := baseInfo.SuperSize
	if budget <= 0 {
		if st, err := os.Stat(baseSuper); err == nil {
			budget = st.Size()
		}
	}
	slots := detectSlots(p.BaseRoot, p.Base.ABUpdate)
	newSuper := filepath.Join(stageDir, "super.img")
	_ = os.Remove(newSuper)
	if err := rebuildSuper(logicalDir, newSuper, budget, slots, "portlab"); err != nil {
		return nil, nil, warnings, err // budget errors already suggest GSI
	}
	if err := util.CopyFile(newSuper, baseSuper); err != nil {
		return nil, nil, warnings, err
	}
	// Drop any stale dat containers for overlaid partitions (super is authoritative).
	for _, name := range overlaid {
		removeStale(workRoot, name, keepSuper)
	}
	_ = donorInfo
	return overlaid, converted, warnings, nil
}

// slottedVariants returns existing logical files for a partition,
// including A/B slot suffixes (system_a.img, system_b.img).
func slottedVariants(logicalDir, name string) []string {
	entries, err := os.ReadDir(logicalDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if n == name+".img" || n == name+"_a.img" || n == name+"_b.img" {
			out = append(out, n)
		}
	}
	return out
}

type keepMode int

const (
	keepBr keepMode = iota
	keepDat
	keepImg
	keepSuper
)

// removeStale deletes competing containers for a partition, keeping exactly
// one family. Mixed .br + .dat + .img for the same partition is ambiguous:
// romtool init prefers .br, so a stale base .br would shadow the fresh
// donor .dat (or vice versa) and produce a wrong image.
func removeStale(workRoot, name string, keep keepMode) {
	br := filepath.Join(workRoot, name+".new.dat.br")
	dat := filepath.Join(workRoot, name+".new.dat")
	tl := filepath.Join(workRoot, name+".transfer.list")
	patch := filepath.Join(workRoot, name+".patch.dat")
	img := filepath.Join(workRoot, name+".img")
	switch keep {
	case keepBr:
		_ = os.Remove(dat)
		_ = os.Remove(img)
	case keepDat:
		_ = os.Remove(br)
		_ = os.Remove(img)
	case keepImg:
		_ = os.Remove(br)
		_ = os.Remove(dat)
		_ = os.Remove(tl)
		_ = os.Remove(patch)
	case keepSuper:
		_ = os.Remove(br)
		_ = os.Remove(dat)
		_ = os.Remove(tl)
		_ = os.Remove(patch)
		_ = os.Remove(img)
	}
	// Incremental deltas always describe the OLD image — never valid after overlay.
	if keep == keepBr || keep == keepDat {
		_ = os.Remove(patch)
	}
}

// refreshOpList updates work op_list sizes from staged raw images (or the
// donor's own op_list sizes) and validates the group budget (refuses overflow).
func refreshOpList(workRoot, stageDir string, overlaid []string, known map[string]int64) error {
	opPath := filepath.Join(workRoot, "dynamic_partitions_op_list")
	op, err := dyn.ParseOpList(opPath)
	if err != nil {
		return fmt.Errorf("work op_list unreadable: %w", err)
	}
	for _, name := range overlaid {
		size := known[name]
		if size <= 0 {
			raw := filepath.Join(stageDir, name+".img")
			if st, err := os.Stat(raw); err == nil && st.Size() > 0 {
				size = alignUp(st.Size(), 4096)
			}
		}
		if size <= 0 {
			// Direct dat overlay without known raw size: keep old resize;
			// romtool repack recalculates from the repacked image.
			continue
		}
		if _, ok := op.Sizes[name]; !ok {
			if err := op.EnsurePartition(name, size); err != nil {
				return err
			}
			continue
		}
		if err := op.SetSize(name, size); err != nil {
			return err
		}
	}
	if err := op.Save(); err != nil {
		return err
	}
	return nil
}

// hardBlockers returns non-autofixed HARD_BRICK/NO_START/NO_BOOT findings,
// minus IDs the pipeline resolves later (e.g. vbmeta at repack).
func hardBlockers(r *audit.Report, ignore map[string]bool) []audit.Finding {
	var out []audit.Finding
	for _, f := range r.Blockers() {
		if f.AutoFixed || ignore[f.ID] {
			continue
		}
		out = append(out, f)
	}
	return out
}
// preflight refuses stages that would fill the disk mid-image (a
// truncated image flashes badly). mult covers transient duplication
// (unpack + repack side by side).
func preflight(dir, root string, mult float64, what string) error {
	need := int64(float64(util.DirSize(root))*mult) + (1 << 30)
	return util.RequireDisk(dir, need, what)
}

// validateWorkDir guards the destructive RemoveAll in Make.
func validateWorkDir(p *project.Project) error {
	if p.Dir == "" || p.Dir == "/" || p.WorkDir == "" {
		return fmt.Errorf("invalid project dir — refusing")
	}
	rel, err := filepath.Rel(p.Dir, p.WorkDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("work dir escapes project — refusing")
	}
	return nil
}

func bootFile(root string) string {
	for _, n := range []string{"boot.img", "init_boot.img"} {
		if c := filepath.Join(root, n); util.FileExists(c) {
			return c
		}
	}
	return ""
}

func hasVendor(root string) bool {
	for _, n := range []string{"vendor.new.dat.br", "vendor.new.dat", "vendor.img", "odm.new.dat.br", "odm.new.dat", "odm.img"} {
		if util.FileExists(filepath.Join(root, n)) {
			return true
		}
	}
	return false
}

// resolveTargetDevice returns a validated device codename for asserts.
func resolveTargetDevice(p *project.Project) (string, error) {
	for _, cand := range []string{p.Meta.Device, p.Base.Device} {
		if cand != "" && cand != "unknown" {
			if err := project.ValidateDevice(cand); err != nil {
				return "", fmt.Errorf("target device %q invalid: %w (fix metadata device)", cand, err)
			}
			return cand, nil
		}
	}
	if len(p.Base.UpdaterDevices) > 0 {
		if err := project.ValidateDevice(p.Base.UpdaterDevices[0]); err == nil {
			return p.Base.UpdaterDevices[0], nil
		}
	}
	return "", fmt.Errorf("target device unknown — set it first: portlab metadata -project <dir> -set device=<codename>")
}

func knownBoth(a, b string) bool {
	la, lb := strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	return la != "" && la != "unknown" && lb != "" && lb != "unknown"
}

func archOK(donor, base string) bool {
	d, b := strings.ToLower(donor), strings.ToLower(base)
	if d == b {
		return true
	}
	return d == "arm" && b == "arm64"
}

// Refine unpacks the work tree via romtool, applies device/treble/SELinux
// patches, repacks, and syncs back — the step that turns a file-level
// merge into a bootable initial port. fstab and ramdisk stay target-owned
// (never overwritten with donor files). Debug props (adb) are always
// enabled: a port without adb visibility cannot be tested.
// On failure work/ is untouched.
func Refine(p *project.Project, permissive bool) ([]string, error) {
	workRoot := filepath.Join(p.WorkDir, "rom_root")
	if !util.FileExists(workRoot) {
		return nil, fmt.Errorf("run make first — no work/rom_root")
	}
	target, err := resolveTargetDevice(p)
	if err != nil {
		return nil, err
	}
	if err := preflight(p.Dir, workRoot, 3.0, "refine (unpack + repack side by side)"); err != nil {
		return nil, err
	}
	tmpDir := filepath.Join(p.Dir, ".refine_proj")
	_ = os.RemoveAll(tmpDir)
	if out, err := util.RunRomtool("init", "-rom", workRoot, "-dir", tmpDir); err != nil {
		return nil, fmt.Errorf("romtool init: %v (%s)", err, short(out))
	}
	applyBasePartitionBudgets(tmpDir, p.BaseRoot)
	success := false
	defer func() {
		if success {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	// Unpack the system family for patching (+boot for ramdisk access).
	// Vendor is deliberately NOT unpacked: its props stay stock and its
	// bytes stay pristine (repacking it would only churn bytes and trip
	// the contamination audit).
	var unpacked []string
	for _, name := range []string{"system", "system_ext", "product"} {
		if out, err := util.RunRomtool("unpack-partition", "-project", tmpDir, "-name", name); err != nil {
			_ = out
			continue
		} else {
			unpacked = append(unpacked, name)
		}
	}
	if len(unpacked) == 0 {
		return nil, fmt.Errorf("no partition unpacked (romtool failed on all) — work/ left untouched")
	}
	bootOK := false
	if out, err := util.RunRomtool("unpack-boot", "-project", tmpDir); err == nil {
		_ = out
		bootOK = true
	}
	notes := applyRefinePatches(tmpDir, target, p.Mode, permissive, unpacked)
	// Repack with self-healing SELinux contexts: donor trees (e.g. MIUI
	// /cust) may contain paths missing from plat_file_contexts, which
	// aborts make_ext4fs. portlab appends partition-appropriate fallback
	// labels and retries — no manual workaround, no romtool change.
	repackNotes, err := repackWithContextHealing(tmpDir)
	notes = append(notes, repackNotes...)
	if err != nil {
		return notes, fmt.Errorf("%w — work/ left untouched", err)
	}
	if bootOK {
		_, _ = util.RunRomtool("repack-boot", "-project", tmpDir)
	}
	// Repair malformed transfer-list totals from the repack (E1001 guard:
	// romtool writes the data-block count as the total; recovery needs the
	// full image block count covering every referenced range).
	if fixed, ferr := dyn.FixTransferLists(filepath.Join(tmpDir, "rom_root")); ferr != nil {
		notes = append(notes, "transfer-list check: "+ferr.Error())
	} else if len(fixed) > 0 {
		notes = append(notes, "transfer-list totals corrected: "+strings.Join(fixed, ", "))
	}
	// Sync the repacked rom_root back (op_list sizes now exact).
	if err := syncBack(filepath.Join(tmpDir, "rom_root"), workRoot); err != nil {
		return notes, fmt.Errorf("sync back: %w — work/ may be partial, re-run make", err)
	}
	// Restore pristine base boot chain to ensure zero contamination drift.
	for _, n := range []string{"boot.img", "init_boot.img", "vendor_boot.img", "recovery.img", "dtbo.img"} {
		bp := filepath.Join(p.BaseRoot, n)
		dp := filepath.Join(workRoot, n)
		if util.FileExists(bp) {
			_ = util.CopyFile(bp, dp)
			if n == "boot.img" {
				bData, err := os.ReadFile(dp)
				if err == nil && !bytes.Contains(bData, []byte("SEANDROIDENFORCE")) {
					if strings.Contains(target, "m20") || strings.Contains(target, "universal") || strings.Contains(target, "exynos") || bytes.Contains(bData, []byte("universal7904")) || bytes.Contains(bData, []byte("samsung")) {
						bData = append(bData, []byte("SEANDROIDENFORCE")...)
						_ = os.WriteFile(dp, bData, 0644)
						notes = append(notes, "boot: appended Samsung SEANDROIDENFORCE magic footer")
					}
				}
			}
		}
	}
	notes = append(notes, "work/rom_root synced with repacked images (op_list exact)")
	success = true
	return notes, nil
}

// applyBasePartitionBudgets overrides partition OriginalSize in rom_project.json
// with the base target device partition capacity if known (e.g. from transfer.list
// or .img). This prevents donor partitions from repacking larger than the physical
// flash partition on the target device, which would cause recovery E1001 aborts.
func applyBasePartitionBudgets(projDir, baseRoot string) {
	projFile := filepath.Join(projDir, "rom_project.json")
	data, err := os.ReadFile(projFile)
	if err != nil {
		return
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		return
	}
	parts, ok := manifest["partitions"].(map[string]any)
	if !ok {
		return
	}
	changed := false
	for name, pObj := range parts {
		pm, ok := pObj.(map[string]any)
		if !ok {
			continue
		}
		tlPath := filepath.Join(baseRoot, name+".transfer.list")
		if _, err := os.Stat(tlPath); err == nil {
			if _, eraseEnd, cmds, err := dyn.ParseTransferList(tlPath); err == nil {
				maxBlock := dyn.MaxRangeEnd(cmds)
				if eraseEnd > maxBlock {
					maxBlock = eraseEnd
				}
				if maxBlock > 0 {
					pm["original_size"] = maxBlock * 4096
					pm["current_size"] = maxBlock * 4096
					changed = true
				}
			}
		} else if imgPath := filepath.Join(baseRoot, name+".img"); util.FileExists(imgPath) {
			if st, err := os.Stat(imgPath); err == nil && st.Size() > 0 {
				pm["original_size"] = st.Size()
				pm["current_size"] = st.Size()
				changed = true
			}
		}
	}
	if changed {
		if out, err := json.MarshalIndent(manifest, "", "  "); err == nil {
			_ = os.WriteFile(projFile, out, 0644)
		}
	}
}

// applyRefinePatches patches unpacked build.props. Pure file edits.
// Vendor props are NEVER spoofed (HAL matching needs them).
func applyRefinePatches(tmpProjDir, target, mode string, permissive bool, unpacked []string) []string {
	var notes []string
	in := func(n string) bool {
		for _, u := range unpacked {
			if u == n {
				return true
			}
		}
		return false
	}
	for _, name := range []string{"system", "system_ext", "product"} {
		if !in(name) {
			continue
		}
		dir := filepath.Join(tmpProjDir, "partitions", name)

		// Neutralize boringssl self-test reboot_on_failure (AOSP init.rc runs
	// FIPS self-tests at early-init and reboots the device when they fail:
	// "reboot,boringssl-self-check-failed"). On ports the tests often fail
	// for benign reasons (SELinux policy skew, missing hw crypto) turning
	// a debuggable TLS issue into an undebuggable bootloop. The tests
	// still RUN; only the forced reboot is removed.
	neutralizeBoringsslReboot(dir, &notes)

		for _, custCand := range []string{
			filepath.Join(dir, "init.miui.cust.rc"),
			filepath.Join(dir, "system", "etc", "init", "init.miui.cust.rc"),
			filepath.Join(dir, "etc", "init", "init.miui.cust.rc"),
		} {
			if util.FileExists(custCand) {
				if content, err := os.ReadFile(custCand); err == nil {
					text := string(content)
					if strings.Contains(text, "wait /dev/block/bootdevice/by-name/cust") {
						text = strings.ReplaceAll(text, "wait /dev/block/bootdevice/by-name/cust", "# wait /dev/block/bootdevice/by-name/cust")
						text = strings.ReplaceAll(text, "mount ext4 /dev/block/bootdevice/by-name/cust", "# mount ext4 /dev/block/bootdevice/by-name/cust")
						_ = os.WriteFile(custCand, []byte(text), 0644)
						notes = append(notes, name+": neutralized non-existent /cust wait/mount")
					}
				}
			}
		}

		cands := []string{
			filepath.Join(dir, "build.prop"),
			filepath.Join(dir, "etc", "build.prop"),
			filepath.Join(dir, "system", "build.prop"),
		}
		for _, cand := range cands {
			if !util.FileExists(cand) {
				continue
			}
			kv := map[string]string{
				"ro.product.device":          target,
				"ro.build.product":           target,
				"ro.debuggable":              "1",
				"ro.secure":                  "0",
				"ro.adb.secure":              "0",
				"persist.sys.usb.config":     "adb",
				"persist.service.adb.enable": "1",
			}
			if mode == "sgsi" || mode == "gsi" {
				kv["ro.treble.enabled"] = "true"
			}
			if permissive {
				kv["ro.boot.selinux"] = "permissive"
			}
			if err := PatchBuildProp(cand, kv); err == nil {
				notes = append(notes, name+": device spoof -> "+target)
			}
			break
		}
	}
	notes = append(notes, "ramdisk/fstab: kept target-owned (not overlaid)")
	return notes
}

// repackWithContextHealing runs romtool repack, parsing
// "cannot lookup security context for /X" failures and appending fallback
// labels to the exact file_contexts romtool used, then retrying.
// Returns notes describing every healing step (transparency: a system
// file gains label entries).
func repackWithContextHealing(tmpDir string) ([]string, error) {
	var notes []string
	for attempt := 0; attempt < 8; attempt++ {
		out, err := util.RunRomtool("repack", "-project", tmpDir)
		if err == nil {
			return notes, nil
		}
		missPath, ctxFile, part, ok := parseMissingContext(out)
		if !ok {
			return notes, fmt.Errorf("romtool repack: %v (%s)", err, short(out))
		}
		if err := appendFallbackContext(ctxFile, missPath, part); err != nil {
			return notes, fmt.Errorf("selinux healing failed for %s: %w", missPath, err)
		}
		notes = append(notes, fmt.Sprintf("selinux: fallback label for %s in %s (retry %d)", missPath, part, attempt+1))
	}
	return notes, fmt.Errorf("romtool repack still failing after context healing")
}

// parseMissingContext extracts the unlabeled path, the -S contexts file,
// and the failing partition from romtool/make_ext4fs output.
func parseMissingContext(out string) (missPath, ctxFile, part string, ok bool) {
	lines := strings.Split(out, "\n")
	curPart := ""
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			if end := strings.Index(t, "]"); end > 1 {
				curPart = t[1:end]
			}
		}
		if i := strings.Index(t, "cannot lookup security context for "); i >= 0 {
			missPath = strings.TrimSpace(t[i+len("cannot lookup security context for "):])
			missPath = strings.Fields(missPath)[0]
			part = curPart
		}
	}
	if missPath == "" {
		return "", "", "", false
	}
	// The -S file romtool passed to make_ext4fs.
	for _, ln := range lines {
		if i := strings.Index(ln, "-S "); i >= 0 {
			rest := strings.Fields(ln[i+3:])
			if len(rest) > 0 {
				ctxFile = rest[0]
				break
			}
		}
	}
	if ctxFile == "" {
		return "", "", "", false
	}
	return missPath, ctxFile, part, true
}

// appendFallbackContext adds "/top(/.*)? u:object_r:<type>:s0" for the
// top-level dir of the unlabeled path. system_file/vendor_file/etc are
// the generic safe defaults for their partitions (unlabeled would deny
// in enforcing mode; a wrong specific label would be worse).
func appendFallbackContext(ctxFile, missPath, part string) error {
	top := missPath
	if i := strings.Index(missPath[1:], "/"); i >= 0 {
		top = missPath[:i+1]
	}
	label := "system_file"
	switch part {
	case "vendor", "vendor_dlkm":
		label = "vendor_file"
	case "odm", "odm_dlkm":
		label = "odm_file"
	case "product":
		label = "product_file"
	case "system_ext":
		label = "system_ext_file"
	}
	entry := fmt.Sprintf("\n%s(/.*)? u:object_r:%s:s0\n", top, label)
	data, err := os.ReadFile(ctxFile)
	if err != nil {
		return err
	}
	if strings.Contains(string(data), top+"(/.*)?") || strings.Contains(string(data), top+" ") {
		return fmt.Errorf("contexts already cover %s yet lookup failed — needs a real sepolicy rule, not a fallback", top)
	}
	return os.WriteFile(ctxFile, append(data, []byte(entry)...), 0644)
}
// neutralizeBoringsslReboot comments out reboot_on_failure in boringssl
// self-test services across init rc files (system/etc/init/hw/init.rc and
// any other rc referencing it). Idempotent: already-neutralized files are
// left untouched.
func neutralizeBoringsslReboot(partDir string, notes *[]string) {
	changed := 0
	_ = filepath.Walk(partDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".rc") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(data)
		if !strings.Contains(text, "boringssl_self_test") || !strings.Contains(text, "reboot_on_failure") {
			return nil
		}
		var out []string
		for _, ln := range strings.Split(text, "\n") {
			t := strings.TrimSpace(ln)
			if strings.HasPrefix(t, "reboot_on_failure") {
				indent := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
				out = append(out, indent+"# portlab: neutered (was: "+t+")")
				changed++
			} else {
				out = append(out, ln)
			}
		}
		_ = os.WriteFile(path, []byte(strings.Join(out, "\n")), 0644)
		return nil
	})
	if changed > 0 {
		*notes = append(*notes, fmt.Sprintf("boringssl: neutered %d reboot_on_failure directive(s) (tests still run)", changed))
	}
}

// syncBack copies repacked rom_root over work rom_root — except the boot
// chain (boot/recovery/vendor_boot/dtbo/vbmeta), which always stays the
// pristine base copy. A magiskboot unpack/repack roundtrip rewrites bytes
// without changing content, and any drift there trips the audit.
func syncBack(src, dst string) error {
	keep := func(name string) bool {
		for _, p := range []string{"boot.img", "init_boot.img", "vendor_boot.img", "recovery.img", "dtbo.img"} {
			if name == p {
				return true
			}
		}
		if strings.HasPrefix(name, "vbmeta") && strings.HasSuffix(name, ".img") {
			return true
		}
		return false
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if keep(e.Name()) {
			continue
		}
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := util.CopyDir(s, d); err != nil {
				return err
			}
			continue
		}
		_ = os.Remove(d)
		if err := util.CopyFile(s, d); err != nil {
			return err
		}
	}
	return nil
}

func refineUnpacked(p *project.Project, mode string, opts Options) []string {
	var notes []string
	for _, name := range []string{"system", "system_ext", "product"} {
		dir := filepath.Join(p.WorkDir, "partitions", name)
		for _, cand := range []string{
			filepath.Join(dir, "build.prop"),
			filepath.Join(dir, "etc", "build.prop"),
			filepath.Join(dir, "system", "build.prop"),
		} {
			if !util.FileExists(cand) {
				continue
			}
			kv := map[string]string{}
			if p.Base.Device != "" && p.Base.Device != "unknown" {
				kv["ro.product.device"] = p.Base.Device
				kv["ro.build.product"] = p.Base.Device
			}
			if opts.Permissive {
				kv["ro.boot.selinux"] = "permissive"
			}
			if mode == "sgsi" || mode == "gsi" {
				kv["ro.treble.enabled"] = "true"
			}
			if len(kv) > 0 && PatchBuildProp(cand, kv) == nil {
				notes = append(notes, "patched "+cand)
			}
			break
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "no unpacked build.prop in work/ — unpack with romtool, edit, then repack")
	}
	return notes
}

// ToGSI builds a standalone system image zip from the donor.
func ToGSI(p *project.Project, outZip string, sgsi bool) error {
	kind := "GSI"
	if sgsi {
		kind = "SGSI"
	}
	_ = os.MkdirAll(filepath.Join(p.Dir, "output"), 0755)
	tmp := filepath.Join(p.Dir, ".gsi_tmp")
	_ = os.MkdirAll(tmp, 0755)
	if err := preflight(p.Dir, p.DonorRoot, 2.0, "to-gsi (extract + zip)"); err != nil {
		return err
	}
	sysImg, err := DonorSystemImage(p.DonorRoot, tmp)
	if err != nil {
		return fmt.Errorf("%s source: %w", kind, err)
	}
	st, _ := os.Stat(sysImg)
	sizeMB := 0.0
	if st != nil {
		sizeMB = float64(st.Size()) / (1024 * 1024)
	}
	if outZip == "" {
		suffix := "GSI"
		if sgsi {
			suffix = "SGSI"
		}
		outZip = filepath.Join(p.Dir, "output", fmt.Sprintf("%s-%s-%s.zip", p.Meta.Name, sanitize(p.Base.Device), suffix))
	}
	stage := filepath.Join(tmp, "stage")
	_ = os.RemoveAll(stage)
	_ = os.MkdirAll(filepath.Join(stage, "META-INF", "com", "google", "android"), 0755)
	_ = util.CopyFile(sysImg, filepath.Join(stage, "system.img"))
	device := p.Base.Device
	if device == "" || device == "unknown" {
		if err := project.ValidateDevice(p.Meta.Device); err == nil {
			device = p.Meta.Device
		} else {
			return fmt.Errorf("target device unknown — set metadata device first (flashing an untargeted GSI risks the wrong device)")
		}
	}
	// Recovery installer ABORTS: GSI is fastboot-only. A no-op "success"
	// script would mislead users into thinking recovery flash worked.
	_ = util.WriteFile(filepath.Join(stage, "META-INF", "com", "google", "android", "updater-script"),
		fmt.Sprintf("abort(\"[%s] %s is fastboot-only for %s (%.1f MB). Flash via flash-%s.sh, not recovery.\");\n",
			kind, p.Meta.Name, device, sizeMB, strings.ToLower(kind)))
	_ = util.WriteFile(filepath.Join(stage, "META-INF", "com", "google", "android", "update-binary"),
		"#!/sbin/sh\necho 'fastboot-only package'\nexit 1\n")
	_ = os.Chmod(filepath.Join(stage, "META-INF", "com", "google", "android", "update-binary"), 0755)
	_ = util.WriteFile(filepath.Join(stage, "flash-"+strings.ToLower(kind)+".sh"), flashScript(kind, device))
	_ = os.Chmod(filepath.Join(stage, "flash-"+strings.ToLower(kind)+".sh"), 0755)
	readme := fmt.Sprintf("%s from donor %s for target %s\nSystem: %s (%.1f MB)\n\nFASTBOOT ONLY (recovery installer aborts on purpose):\n  ./flash-%s.sh\n\nManual:\n  fastboot reboot fastboot\n  fastboot flash system system.img\n  fastboot reboot\n\nIf verity blocks boot, flash your stock vbmeta with verity disabled.\n", kind, p.Donor.Device, device, filepath.Base(sysImg), sizeMB, strings.ToLower(kind))
	if sgsi {
		readme += "SGSI: Treble/Phh overlays; add vendor RIL/audio/camera shims.\n"
	}
	_ = util.WriteFile(filepath.Join(stage, "README-"+kind+".txt"), readme)
	if err := util.CreateZip(stage, outZip); err != nil {
		return err
	}
	if sgsi {
		p.Mode = "sgsi"
	} else {
		p.Mode = "gsi"
	}
	p.LastReport = outZip
	return p.Save()
}

// DonorSystemImage resolves donor system.img via direct file, donor
// super.img (lpunpack), romtool orchestration (dat/br), or payload-dumper.
func DonorSystemImage(donorRoot, tmp string) (string, error) {
	if cand := filepath.Join(donorRoot, "system.img"); util.FileExists(cand) {
		if _, err := dyn.ValidateImage(cand, 1<<20); err != nil {
			return "", fmt.Errorf("donor system.img: %w", err)
		}
		return cand, nil
	}
	if util.FileExists(filepath.Join(donorRoot, "super.img")) {
		outDir := filepath.Join(tmp, "donor_super")
		if err := unpackSuper(donorRoot, outDir); err != nil {
			return "", err
		}
		if cand := filepath.Join(outDir, "system.img"); util.FileExists(cand) {
			if _, err := dyn.ValidateImage(cand, 1<<20); err != nil {
				return "", fmt.Errorf("donor super system: %w", err)
			}
			return cand, nil
		}
		return "", fmt.Errorf("donor super.img has no system logical partition")
	}
	// Try romtool orchestration: init temp project + unpack system.
	if projDir := filepath.Join(tmp, "donor_proj"); true {
		_ = os.RemoveAll(projDir)
		if out, err := util.RunRomtool("init", "-rom", donorRoot, "-dir", projDir); err == nil {
			_ = out
			if out2, err2 := util.RunRomtool("unpack-partition", "-project", projDir, "-name", "system"); err2 == nil {
				_ = out2
				if cand := filepath.Join(projDir, "raw_images", "system.img"); util.FileExists(cand) {
					return cand, nil
				}
			}
		}
	}
	// Direct payload fallback.
	if util.FileExists(filepath.Join(donorRoot, "payload.bin")) {
		out := filepath.Join(tmp, "payload_out")
		_ = os.MkdirAll(out, 0755)
		if _, err := util.RunPayload( "-o", out, "-p", "system", filepath.Join(donorRoot, "payload.bin")); err != nil {
			return "", err
		}
		if util.FileExists(filepath.Join(out, "system.img")) {
			return filepath.Join(out, "system.img"), nil
		}
	}
	return "", fmt.Errorf("no system.img resolvable (need system.img or unpackable system dat/payload)")
}

// BootChecks inspects work/rom_root (or base) for boot blockers,
// validating content — not just file existence. Any FAIL means the
// output must not be flashed.
func BootChecks(p *project.Project) []BootCheck {
	root := filepath.Join(p.WorkDir, "rom_root")
	if !util.FileExists(root) {
		root = p.BaseRoot
	}
	var out []BootCheck
	pass := func(n, d string) { out = append(out, BootCheck{n, "PASS", d}) }
	warn := func(n, d string) { out = append(out, BootCheck{n, "WARN", d}) }
	fail := func(n, d string) { out = append(out, BootCheck{n, "FAIL", d}) }
	info := dyn.Detect(root)
	probe := filepath.Join(p.Dir, ".probe")

	if bf := bootFile(root); bf != "" {
		if err := dyn.ValidateBootImg(bf); err != nil {
			fail("kernel/boot", "target boot corrupt: "+err.Error())
		} else {
			pass("kernel/boot", "target boot present and valid (donor kernel excluded)")
		}
	} else if pb := payloadBoot(root, probe); pb != "" {
		if err := dyn.ValidateBootImg(pb); err != nil {
			fail("kernel/boot", "payload boot corrupt: "+err.Error())
		} else {
			pass("kernel/boot", "target boot present in payload (materialized at make)")
		}
	} else {
		fail("kernel/boot", "no boot.img — unbootable without target kernel")
	}
	if !vendorPresent(root, probe) {
		fail("vendor/odm", "no target vendor/odm — HALs missing, guaranteed bootloop")
	} else {
		pass("vendor/odm", "target vendor/odm present")
	}
	// System source must be valid in the layout's own terms.
	sysOK := false
	for _, n := range []string{"system", "system_ext", "product"} {
		switch dyn.Format(root, n) {
		case dyn.SrcDat, dyn.SrcDatBr:
			if err := dyn.ValidateDatPair(root, n); err != nil {
				fail("system/"+n, err.Error())
			} else {
				sysOK = true
			}
		case dyn.SrcImg:
			if _, err := dyn.ValidateImage(filepath.Join(root, n+".img"), 1<<20); err != nil {
				fail("system/"+n, err.Error())
			} else {
				sysOK = true
			}
		}
	}
	if !sysOK && info.Layout != dyn.LayoutSuper {
		fail("system", "no valid donor system in output")
	}
	switch info.Layout {
	case dyn.LayoutDynamicDat:
		op, err := dyn.ParseOpList(filepath.Join(root, "dynamic_partitions_op_list"))
		if err != nil {
			fail("dynamic/op_list", "unreadable op_list: "+err.Error())
			break
		}
		ok := true
		for name, want := range op.Sizes {
			actual, aerr := partitionBackingSize(root, name)
			if aerr != nil || actual <= 0 {
				warn("dynamic/"+name, "no backing file yet — size recalculated at repack")
				continue
			}
			if actual != want {
				warn("dynamic/"+name, fmt.Sprintf("op_list says %d but file is %d — recalculated at repack", want, actual))
				ok = false
			}
		}
		fit := true
		for gname, use := range op.GroupUsage() {
			if use[1] > 0 && use[0] > use[1] {
				fail("dynamic/"+gname, fmt.Sprintf("group over budget %d > %d — must shrink or use GSI", use[0], use[1]))
				fit = false
			}
		}
		if ok && fit {
			pass("dynamic/op_list", "op_list sizes match files, groups fit")
		}
	case dyn.LayoutSuper:
		if _, err := dyn.ValidateImage(filepath.Join(root, "super.img"), 1<<20); err != nil {
			fail("dynamic/super", "super.img invalid: "+err.Error())
		} else {
			pass("dynamic/super", "super.img present and valid")
		}
	case dyn.LayoutNonDynamic, dyn.LayoutRaw:
		pass("layout", "non-dynamic/raw layout — mounts come from target updater-script")
	default:
		warn("layout", "unrecognized layout — verify before flashing")
	}
	if util.FileExists(filepath.Join(root, "vbmeta.img")) {
		pass("avb", "vbmeta present (repack disables verification)")
	} else {
		warn("avb", "no vbmeta — verity may still enforce on some devices")
	}
	updater := filepath.Join(root, "META-INF", "com", "google", "android", "updater-script")
	if util.FileExists(updater) {
		if data, err := os.ReadFile(updater); err == nil && strings.Contains(string(data), p.Base.Device) {
			pass("updater-script", "assert targets "+p.Base.Device)
		} else {
			warn("updater-script", "assert missing target "+p.Base.Device)
		}
	} else {
		warn("updater-script", "no updater-script — not recovery-flashable yet")
	}
	if util.FileExists(filepath.Join(p.WorkDir, "partitions", "system", "build.prop")) {
		pass("build.prop", "unpacked system available for refine edits")
	} else {
		warn("build.prop", "system not unpacked — unpack to edit fingerprint/fstab")
	}
	return out
}

// partitionBackingSize returns the raw image bytes backing a partition:
// .img file size, or 0 when dat-family (raw size known only after unpack).
func partitionBackingSize(root, name string) (int64, error) {
	if c := filepath.Join(root, name+".img"); util.FileExists(c) {
		st, err := os.Stat(c)
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	}
	return 0, fmt.Errorf("no raw backing file")
}

// Repack builds the final flashable zip via romtool orchestration,
// then validates the output before handing it back. An unvalidated zip
// is how bad flashes happen — validation failures delete the zip.
func Repack(p *project.Project, outZip string) (string, error) {
	workRoot := filepath.Join(p.WorkDir, "rom_root")
	if !util.FileExists(workRoot) {
		return "", fmt.Errorf("run make first — no work/rom_root")
	}
	// Pre-validate work tree: refuse to zip a tree that cannot boot.
	if bf := bootFile(workRoot); bf == "" {
		return "", fmt.Errorf("work has no boot.img — refusing to pack an unbootable zip")
	} else if err := dyn.ValidateBootImg(bf); err != nil {
		return "", fmt.Errorf("work boot corrupt: %w — refusing", err)
	}
	if !hasVendor(workRoot) {
		return "", fmt.Errorf("work has no vendor/odm — refusing to pack a HAL-less zip")
	}
	if outZip == "" {
		_ = os.MkdirAll(filepath.Join(p.Dir, "output"), 0755)
		outZip = filepath.Join(p.Dir, "output", fmt.Sprintf("%s-port-%s.zip", p.Meta.Name, time.Now().Format("20060102_150405")))
	}
	// Pre-repack audit (vbmeta-enabled expected: romtool disables it now).
	if arep := audit.Audit(p); len(hardBlockers(arep, map[string]bool{"vbmeta-enabled": true})) > 0 {
		b := hardBlockers(arep, map[string]bool{"vbmeta-enabled": true})
		return "", fmt.Errorf("audit blocker [%s] %s: %s — refusing to pack", b[0].Severity, b[0].ID, b[0].Detail)
	}
	if err := preflight(p.Dir, workRoot, 2.0, "repack (temp project + output zip)"); err != nil {
		return "", err
	}
	tmpProj := filepath.Join(p.Dir, ".repack_proj")
	_ = os.RemoveAll(tmpProj)
	if out, err := util.RunRomtool("init", "-rom", workRoot, "-dir", tmpProj); err != nil {
		return "", fmt.Errorf("romtool init: %v (%s)", err, out)
	}
	if out, err := util.RunRomtool("repack-rom", "-project", tmpProj, "-output", outZip); err != nil {
		return "", fmt.Errorf("romtool repack-rom: %v (%s)", err, out)
	}
	if err := ValidateOutputZip(outZip, p.Base.Device); err != nil {
		_ = os.Remove(outZip)
		return "", fmt.Errorf("output failed validation and was deleted: %w", err)
	}
	return outZip, nil
}

// ValidateOutputZip ensures a finished zip is flashable and targeted.
func ValidateOutputZip(zipPath, device string) error {
	st, err := os.Stat(zipPath)
	if err != nil || st.Size() < 1024 {
		return fmt.Errorf("zip missing or trivially small")
	}
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("zip unreadable: %w", err)
	}
	defer r.Close()
	var hasSystem, hasVendor, hasBoot, hasUpdater bool
	var updaterOK bool
	datNeedsTransfer := map[string]bool{}
	hasTransfer := map[string]bool{}
	for _, f := range r.File {
		n := f.Name
		if strings.HasSuffix(n, ".new.dat.br") || strings.HasSuffix(n, ".new.dat") {
			if !strings.HasSuffix(n, ".patch.dat") {
				datNeedsTransfer[strings.TrimSuffix(strings.TrimSuffix(n, ".new.dat.br"), ".new.dat")] = true
			}
		}
		if strings.HasSuffix(n, ".transfer.list") {
			hasTransfer[strings.TrimSuffix(n, ".transfer.list")] = true
		}
		switch {
		case n == "system.new.dat.br" || n == "system.new.dat" || n == "system.img":
			hasSystem = true
		case strings.HasPrefix(n, "vendor.new.dat") || n == "vendor.img" || strings.HasPrefix(n, "odm."):
			hasVendor = true
		case n == "boot.img" || n == "init_boot.img":
			hasBoot = true
		case n == "META-INF/com/google/android/updater-script":
			hasUpdater = true
			rc, err := f.Open()
			if err == nil {
				buf := make([]byte, 8192)
				m, _ := rc.Read(buf)
				rc.Close()
				if device != "" && device != "unknown" && strings.Contains(string(buf[:m]), device) {
					updaterOK = true
				}
			}
		}
	}
	if !hasSystem {
		return fmt.Errorf("zip has no system image")
	}
	if !hasVendor {
		return fmt.Errorf("zip has no vendor/odm — would bootloop")
	}
	if !hasBoot {
		return fmt.Errorf("zip has no boot.img — unbootable")
	}
	if !hasUpdater {
		return fmt.Errorf("zip has no updater-script — not recovery-flashable")
	}
	if device != "" && device != "unknown" && !updaterOK {
		return fmt.Errorf("updater-script does not assert %s — wrong-device flash risk", device)
	}
	for name := range datNeedsTransfer {
		if !hasTransfer[name] {
			return fmt.Errorf("%s has dat but zip lacks transfer.list — would flash garbage", name)
		}
	}
	return nil
}

// --- helpers ---

func FindPartitionFiles(romRoot, name string) []string {
	var out []string
	for _, cand := range []string{
		filepath.Join(romRoot, name+".new.dat.br"),
		filepath.Join(romRoot, name+".new.dat"),
		filepath.Join(romRoot, name+".transfer.list"),
		filepath.Join(romRoot, name+".img"),
	} {
		if util.FileExists(cand) {
			out = append(out, cand)
		}
	}
	return out
}

func ensurePayloadReady(donorRoot string, want []string) error {
	pb := filepath.Join(donorRoot, "payload.bin")
	if !util.FileExists(pb) {
		return nil
	}
	need := false
	for _, w := range want {
		if !util.FileExists(filepath.Join(donorRoot, w+".img")) {
			need = true
		}
	}
	if !need {
		return nil
	}
	tmp := filepath.Join(donorRoot, ".payload_out")
	_ = os.MkdirAll(tmp, 0755)
	if _, err := util.RunPayload( "-o", tmp, "-p", strings.Join(want, ","), pb); err != nil {
		return err
	}
	for _, w := range want {
		if util.FileExists(filepath.Join(tmp, w+".img")) {
			_ = util.CopyFile(filepath.Join(tmp, w+".img"), filepath.Join(donorRoot, w+".img"))
		}
	}
	return nil
}

func PatchBuildProp(path string, kv map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	seen := map[string]bool{}
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") || !strings.Contains(t, "=") {
			continue
		}
		k := strings.TrimSpace(strings.SplitN(t, "=", 2)[0])
		if v, ok := kv[k]; ok {
			lines[i] = k + "=" + v
			seen[k] = true
		}
	}
	for k, v := range kv {
		if !seen[k] {
			lines = append(lines, k+"="+v)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

func RewriteUpdater(scriptPath, target string) error {
	if err := project.ValidateDevice(target); err != nil {
		return fmt.Errorf("refusing to write unsafe device assert: %w", err)
	}
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		return err
	}
	assert := fmt.Sprintf(`getprop("ro.product.device") == "%s" || abort("This package is for device: %s; this device is " + getprop("ro.product.device") + ".");`, target, target)
	lines := strings.Split(string(data), "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.Contains(t, "getprop") && strings.Contains(t, "abort") &&
			(strings.Contains(t, "ro.product.device") || strings.Contains(t, "ro.build.product")) {
			lines[i] = assert
			return os.WriteFile(scriptPath, []byte(strings.Join(lines, "\n")), 0644)
		}
	}
	lines = append([]string{assert}, lines...)
	return os.WriteFile(scriptPath, []byte(strings.Join(lines, "\n")), 0644)
}

// flashScript is a device-checked fastbootd flash script. It verifies the
// connected device before touching partitions — flashing the wrong device
// is a hard-brick vector. No vbmeta line: this zip ships no vbmeta, and a
// flash line for a missing file would only confuse.
func flashScript(kind, device string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
# %s for %s — fastboot only. Read the README first.
set -e
PRODUCT="$(fastboot getvar product 2>&1 | grep -o 'product: .*' | cut -d' ' -f2 | tr -d '\r')"
if [ "$PRODUCT" != "%s" ]; then
  echo "Connected device is '$PRODUCT', expected '%s'. Refusing."
  echo "Set the right device or edit the script knowingly."
  exit 1
fi
read -r -p "Flash %s system.img to $PRODUCT? [y/N] " yn
case "$yn" in [Yy]*) ;; *) echo "Aborted."; exit 1;; esac
fastboot reboot fastboot
fastboot flash system system.img
fastboot reboot
`, kind, device, device, device, kind)
}

func PortNotes(p *project.Project, mode string, overlaid, warnings []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Port: %s\nDonor: %s (%s)\nTarget: %s (%s)\nMode: %s\n\n",
		p.Meta.Name, p.DonorSrc, p.Donor.Device, p.BaseSrc, p.Base.Device, mode))
	b.WriteString("Taken from donor: " + strings.Join(overlaid, ", ") + "\nKept from target: vendor, odm, boot, vendor_boot, dtbo, vbmeta, firmware, META-INF\n\n")
	b.WriteString("Refine loop:\n  1. portlab check -project <dir>\n  2. romtool unpack/edit/repack work (build.prop, fstab, permissions)\n  3. portlab repack -project <dir>\n  4. flash + adb logcat + repeat\n\n")
	if len(warnings) > 0 {
		b.WriteString("Warnings:\n")
		for _, w := range warnings {
			b.WriteString("  - " + w + "\n")
		}
	}
	return b.String()
}

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "unknown" {
		return "target"
	}
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	return s
}

func SortedKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
