// Deep audit runs on unpacked trees (system + vendor + boot ramdisk).
// This is where cross-SoC boot killers live: Treble HAL binding is the
// #1 cause of bootloops when the donor framework asks for interfaces the
// target vendor does not provide. These checks cannot run at file level
// (HAL manifests live inside the images), so DeepAudit unpacks via romtool
// into a temp project, checks, and cleans up. Findings are report-only:
// missing HALs cannot be auto-fixed, only detected with precision.
package audit

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"portlab/internal/dyn"
	"portlab/internal/project"
	"portlab/internal/util"
)

// HalReq is one required or provided HAL interface. Matrix entries list
// ALTERNATIVE versions (device may implement any); manifests list what the
// vendor provides. Optional is true only if every occurrence is optional.
type HalReq struct {
	Name     string
	Versions []string
	Optional bool
}

// vintfHal mirrors <hal> in manifests and matrices.
type vintfHal struct {
	Format    string   `xml:"format,attr"`
	Optional  string   `xml:"optional,attr"`
	Name      string   `xml:"name"`
	Versions  []string `xml:"version"`
	FQNames   []string `xml:"fqname"`
}

type vintfDoc struct {
	Hals []vintfHal `xml:"hal"`
}

// ParseVintfDoc parses a manifest or compatibility matrix into name->req.
func ParseVintfDoc(path string) (map[string]HalReq, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc vintfDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("bad vintf xml %s: %w", filepath.Base(path), err)
	}
	out := map[string]HalReq{}
	for _, h := range doc.Hals {
		name := strings.TrimSpace(h.Name)
		if name == "" {
			continue
		}
		vers := append(append([]string{}, h.Versions...), fqVersions(h.FQNames)...)
		cur := out[name]
		cur.Name = name
		for _, v := range vers {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if !containsVer(cur.Versions, v) {
				cur.Versions = append(cur.Versions, v)
			}
		}
		// Optional only if every occurrence says optional.
		if _, seen := out[name]; !seen {
			cur.Optional = h.Optional == "true"
		} else {
			cur.Optional = cur.Optional && h.Optional == "true"
		}
		out[name] = cur
	}
	return out, nil
}

func containsVer(in []string, s string) bool {
	for _, x := range in {
		if x == s {
			return true
		}
	}
	return false
}

// fqVersions extracts versions from <fqname>android.hardware.x@1.0::IFoo/default</fqname>.
func fqVersions(fq []string) []string {
	var out []string
	for _, s := range fq {
		if i := strings.Index(s, "@"); i >= 0 {
			rest := s[i+1:]
			if j := strings.Index(rest, ":"); j >= 0 {
				rest = rest[:j]
			}
			out = append(out, strings.TrimSpace(rest))
		}
	}
	return out
}

// minRequired takes the floor of a range ("2.1-2.3" -> "2.1").
func minRequired(v string) string {
	if i := strings.Index(v, "-"); i >= 0 {
		return strings.TrimSpace(v[:i])
	}
	return strings.TrimSpace(v)
}

func maxVersion(vs []string) string {
	best := ""
	for _, v := range vs {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if best == "" || versionLess(best, v) {
			best = v
		}
	}
	return best
}

// versionLess compares dotted versions ("1" vs "2.1").
func versionLess(a, b string) bool {
	pa, pb := splitVer(a), splitVer(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func splitVer(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ".") {
		n, _ := strconv.Atoi(strings.TrimSpace(f))
		out = append(out, n)
	}
	return out
}

// VendorManifests lists device HAL manifests in an unpacked vendor tree,
// including manifest fragments in etc/vintf/manifest/.
func VendorManifests(vendorDir string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(c string) {
		if util.FileExists(c) && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	vd := filepath.Join(vendorDir, "etc", "vintf")
	add(filepath.Join(vd, "manifest.xml"))
	_ = filepath.Walk(vd, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if strings.HasSuffix(base, ".xml") && !strings.HasPrefix(base, "compatibility_matrix") {
			add(path)
		}
		return nil
	})
	return out
}

// FrameworkMatrices lists framework compatibility matrices in unpacked system.
func FrameworkMatrices(systemDir string) []string {
	var out []string
	vd := filepath.Join(systemDir, "etc", "vintf")
	entries, err := os.ReadDir(vd)
	if err != nil {
		// system-as-root layouts nest under system/system.
		vd = filepath.Join(systemDir, "system", "etc", "vintf")
		entries, err = os.ReadDir(vd)
	}
	if err != nil {
		return out
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "compatibility_matrix") && strings.HasSuffix(e.Name(), ".xml") {
			out = append(out, filepath.Join(vd, e.Name()))
		}
	}
	return out
}

// HalGap is one framework requirement the vendor does not satisfy.
type HalGap struct {
	Name     string
	Required string
	Provided string // "" when absent
	Optional bool
}

// CompareHal returns gaps: framework requirements no provided version
// satisfies. A requirement is satisfied when the vendor implements any
// listed version at same-or-newer minor within the same major.
func CompareHal(required, provided map[string]HalReq) []HalGap {
	var gaps []HalGap
	for name, req := range required {
		prov, ok := provided[name]
		best := ""
		if ok {
			best = satisfiesAny(req.Versions, prov.Versions)
		}
		if best == "" {
			gaps = append(gaps, HalGap{name, joinVersions(req.Versions), joinVersions(prov.Versions), req.Optional})
		}
	}
	return gaps
}

// satisfiesAny returns the provided version that satisfies a requirement,
// or "" when none does.
func satisfiesAny(required, provided []string) string {
	for _, r := range required {
		r = minRequired(r)
		for _, p := range provided {
			if versionSatisfies(minRequired(p), r) {
				return p
			}
		}
	}
	return ""
}

// versionSatisfies reports provided >= required. AIDL versions are single
// integers (higher covers lower); HIDL is major.minor (same major, minor >=).
func versionSatisfies(prov, req string) bool {
	pp, rp := splitVer(prov), splitVer(req)
	if len(pp) == 1 && len(rp) == 1 {
		return pp[0] >= rp[0]
	}
	if len(pp) == 0 || len(rp) == 0 || pp[0] != rp[0] {
		return false
	}
	return !versionLess(prov, req)
}

func joinVersions(vs []string) string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vs {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return strings.Join(out, "|")
}

// HalSeverity grades a HAL gap. Display + boot-control + FBE creds block
// boot; the rest degrade features. Wording stays qualified: these are the
// worst plausible outcomes seen in the field. ab selects A/B relevance.
func HalSeverity(name string, ab bool) (Severity, string) {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "graphics.composer"), strings.Contains(n, "graphics.mapper"), strings.Contains(n, "graphics.allocator"):
		return NoBoot, "display pipeline — bootanimation/SystemUI loop likely"
	case strings.Contains(n, "hardware.boot"):
		if ab {
			return NoBoot, "slot control — A/B devices bootloop without it"
		}
		return Degraded, "slot control unused on A-only devices"
	case strings.Contains(n, "keymaster"), strings.Contains(n, "keymint"), strings.Contains(n, "gatekeeper"):
		return NoBoot, "credential/FBE unlock may fail on encrypted data"
	case strings.Contains(n, "health"):
		return Degraded, "charger mode + battery stats affected"
	case strings.Contains(n, "vibrator"), strings.Contains(n, "light"), strings.Contains(n, "power"):
		return Degraded, "haptics/lights/power hints affected"
	default:
		return Degraded, "feature using this HAL will break (camera/audio/sensors/RIL/biometrics class)"
	}
}

// VintfAudit compares donor framework matrices against target vendor
// manifests, reporting only REGRESSIONS versus the donor's own vendor.
// Absolute gaps are noise: stock ROMs routinely lack tv/automotive/vr
// HALs and boot fine. Without a donor-vendor baseline every gap is
// reported with a no-baseline note.
func VintfAudit(systemDir, vendorDir, donorVendorDir string, ab bool) []Finding {
	var out []Finding
	mats := FrameworkMatrices(systemDir)
	mans := VendorManifests(vendorDir)
	if len(mats) == 0 {
		out = append(out, Finding{"vintf-no-matrix", NoBoot, "hal", "no framework compatibility matrix in donor system — cannot verify HAL requirements", false, "unpack system fully, then re-run audit -deep"})
		return out
	}
	if len(mans) == 0 {
		out = append(out, Finding{"vintf-no-manifest", NoBoot, "hal", "no vendor HAL manifest in target vendor — cannot verify provided interfaces", false, "use a fuller stock vendor as base"})
		return out
	}
	required := unionDocs(mats)
	provided := unionDocs(mans)
	gaps := CompareHal(required, provided)
	regressions := gaps
	baseline := false
	if donorVendorDir != "" {
		if donorMans := VendorManifests(donorVendorDir); len(donorMans) > 0 {
			donorProvided := unionDocs(donorMans)
			donorGaps := CompareHal(required, donorProvided)
			donorGapSet := map[string]bool{}
			for _, g := range donorGaps {
				donorGapSet[g.Name] = true
			}
			var reg []HalGap
			for _, g := range gaps {
				if !donorGapSet[g.Name] {
					reg = append(reg, g)
				}
			}
			regressions = reg
			baseline = true
		}
	}
	if len(regressions) == 0 {
		if baseline {
			out = append(out, Finding{"vintf-ok", Info, "hal",
				fmt.Sprintf("no HAL regressions vs donor stock (%d donor gaps also present in stock, all benign)", len(gaps)), true, ""})
		} else {
			out = append(out, Finding{"vintf-ok", Info, "hal",
				fmt.Sprintf("all %d framework HAL requirements satisfied by vendor", len(required)), true, ""})
		}
		return out
	}
	for _, g := range regressions {
		sev, why := HalSeverity(g.Name, ab)
		if g.Optional {
			sev = Degraded
		}
		note := ""
		if !baseline {
			note = " (no donor-vendor baseline — may be benign if stock lacks it too)"
		}
		detail := fmt.Sprintf("HAL %s requires >= %s, vendor has %s — %s%s", g.Name, g.Required, orNone(g.Provided), why, note)
		out = append(out, Finding{"hal/" + g.Name, sev, "hal", detail, false,
			"port that HAL from stock, shim via SGSI overlay, or accept the broken feature"})
	}
	return out
}

// unionDocs merges several vintf docs (matrices or manifests).
func unionDocs(paths []string) map[string]HalReq {
	merged := map[string]HalReq{}
	for _, m := range paths {
		rm, err := ParseVintfDoc(m)
		if err != nil {
			continue
		}
		for k, v := range rm {
			cur := merged[k]
			cur.Name = k
			for _, ver := range v.Versions {
				if !containsVer(cur.Versions, ver) {
					cur.Versions = append(cur.Versions, ver)
				}
			}
			if _, seen := merged[k]; !seen {
				cur.Optional = v.Optional
			} else {
				cur.Optional = cur.Optional && v.Optional
			}
			merged[k] = cur
		}
	}
	return merged
}

func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

// FstabAudit verifies every system/vendor/product/odm mount in target
// ramdisk + vendor fstabs resolves to a partition present in the output.
func FstabAudit(fstabDirs []string, have func(string) bool) []Finding {
	var out []Finding
	type mount struct {
		point string
		file  string
	}
	var mounts []mount
	seen := map[string]bool{}
	for _, dir := range fstabDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "fstab") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) < 2 {
					continue
				}
				mp := fields[1]
				switch mp {
				case "/system", "/vendor", "/product", "/odm", "/system_ext":
					key := mp
					if !seen[key] {
						seen[key] = true
						mounts = append(mounts, mount{mp, e.Name()})
					}
				}
			}
		}
	}
	if len(mounts) == 0 {
		out = append(out, Finding{"fstab-none", NoBoot, "fstab", "no system/vendor/product fstab entries found — cannot verify mounts", false, "check ramdisk unpack included fstab files"})
		return out
	}
	for _, m := range mounts {
		part := strings.TrimPrefix(m.point, "/")
		if have(part) {
			out = append(out, Finding{"fstab/" + part, Info, "fstab", m.point + " mounts to a present partition (" + m.file + ")", true, ""})
		} else {
			out = append(out, Finding{"fstab/" + part, NoBoot, "fstab", m.point + " has no partition in output — init will fail the mount", false, "add the partition or fix " + m.file})
		}
	}
	return out
}

// PropsAudit cross-checks unpacked build.props.
func PropsAudit(systemProps, vendorProps map[string]string, target string) []Finding {
	var out []Finding
	get := func(m map[string]string, keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(m[k]); v != "" {
				return v
			}
		}
		return ""
	}
	if dev := get(systemProps, "ro.product.device", "ro.build.product"); dev != "" && dev != target {
		out = append(out, Finding{"props-device", NoBoot, "props",
			"system fingerprint still claims " + dev + " (target " + target + ") — fingerprint-gated services fail", false,
			"run refine to spoof device"})
	} else if dev != "" {
		out = append(out, Finding{"props-device-ok", Info, "props", "system device spoofed to " + target, true, ""})
	}
	if fp := get(systemProps, "ro.system.build.fingerprint", "ro.build.fingerprint"); fp != "" && !strings.Contains(fp, target) {
		out = append(out, Finding{"props-fingerprint", Degraded, "props",
			"build fingerprint names another device — SafetyNet/Play Integrity and OTAs affected", false,
			"rebuild fingerprint for target in refine follow-ups"})
	}
	if vndk := get(systemProps, "ro.vndk.version"); vndk != "" {
		if vv := get(vendorProps, "ro.vndk.version", "ro.vendor.vndk.version"); vv != "" && vv != vndk {
			out = append(out, Finding{"props-vndk", Degraded, "props",
				"system VNDK " + vndk + " vs vendor " + vv + " — linker-namespace friction possible", false,
				"SGSI shims or vendor-matched donor"})
		}
	}
	return out
}

// ContextsAudit checks SELinux file_contexts needed for a clean repack.
func ContextsAudit(systemDir, vendorDir string) []Finding {
	var out []Finding
	plat := []string{
		filepath.Join(systemDir, "etc", "selinux", "plat_file_contexts"),
		filepath.Join(systemDir, "system", "etc", "selinux", "plat_file_contexts"),
	}
	found := false
	for _, c := range plat {
		if util.FileExists(c) {
			found = true
			break
		}
	}
	if !found {
		out = append(out, Finding{"selinux-plat", NoBoot, "sepolicy", "no plat_file_contexts — repacked image mislabeled, init denials likely", false, "unpack full system, keep contexts"})
	} else {
		out = append(out, Finding{"selinux-plat-ok", Info, "sepolicy", "platform file_contexts present", true, ""})
	}
	vendor := false
	if entries, err := os.ReadDir(filepath.Join(vendorDir, "etc", "selinux")); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), "_file_contexts") {
				vendor = true
				break
			}
		}
	}
	if !vendor {
		out = append(out, Finding{"selinux-vendor", Degraded, "sepolicy", "no vendor file_contexts — vendor labels fall back to platform", false, "verify with a full vendor unpack"})
	}
	return out
}

// DeepAudit runs the file-level Audit plus unpacked-tree checks (HAL,
// fstab, props, sepolicy, kernel modules) via a temp romtool project.
// Slow (unpacks GB images) — explicit command only. Temp data is removed.
func DeepAudit(p *project.Project) (*Report, error) {
	r := Audit(p)
	defer os.RemoveAll(filepath.Join(p.Dir, ".deep_baseline"))
	workRoot := filepath.Join(p.WorkDir, "rom_root")
	if !util.FileExists(workRoot) {
		return r, fmt.Errorf("run make first — no work/rom_root")
	}
	if err := util.RequireDisk(p.Dir, util.DirSize(workRoot)*2+(1<<30), "deep audit (full unpack)"); err != nil {
		return r, err
	}
	tmpDir := filepath.Join(p.Dir, ".deep_proj")
	_ = os.RemoveAll(tmpDir)
	if out, err := util.RunRomtool("init", "-rom", workRoot, "-dir", tmpDir); err != nil {
		return r, fmt.Errorf("romtool init: %v (%s)", err, shortOut(out))
	}
	defer os.RemoveAll(tmpDir)
	for _, name := range []string{"system", "vendor", "product", "system_ext", "vendor_dlkm", "odm_dlkm"} {
		_, _ = util.RunRomtool("unpack-partition", "-project", tmpDir, "-name", name)
	}
	_, _ = util.RunRomtool("unpack-boot", "-project", tmpDir)
	part := func(n string) string { return filepath.Join(tmpDir, "partitions", n) }
	ab := p.Base.ABUpdate == "true" || util.FileExists(filepath.Join(workRoot, "vendor_boot.img"))
	r.Findings = append(r.Findings, VintfAudit(part("system"), part("vendor"), donorVendorBaseline(p), ab)...)
	r.Findings = append(r.Findings, PropsAudit(
		project.ParseBuildProp(firstExisting(
			filepath.Join(part("system"), "build.prop"),
			filepath.Join(part("system"), "etc", "build.prop"),
			filepath.Join(part("system"), "system", "build.prop"),
		)),
		project.ParseBuildProp(firstExisting(
			filepath.Join(part("vendor"), "build.prop"),
			filepath.Join(part("vendor"), "etc", "build.prop"),
		)),
		targetOf(p),
	)...)
	r.Findings = append(r.Findings, ContextsAudit(part("system"), part("vendor"))...)
	have := func(name string) bool {
		if dyn.Format(workRoot, name) != dyn.SrcMissing {
			return true
		}
		for _, cand := range []string{
			filepath.Join(part(name), "build.prop"),
			filepath.Join(part(name), "etc", "build.prop"),
		} {
			if util.FileExists(cand) {
				return true
			}
		}
		return false
	}
	var fstabDirs []string
	if entries, err := os.ReadDir(filepath.Join(tmpDir, "boot")); err == nil {
		for _, e := range entries {
			fstabDirs = append(fstabDirs, filepath.Join(tmpDir, "boot", e.Name(), "ramdisk"))
		}
	}
	fstabDirs = append(fstabDirs, filepath.Join(part("vendor"), "etc"))
	r.Findings = append(r.Findings, FstabAudit(fstabDirs, have)...)
	r.Findings = append(r.Findings, KoAudit([]string{
		filepath.Join(part("vendor"), "lib", "modules"),
		part("vendor_dlkm"), part("odm_dlkm"),
	}, kernelReleaseOf(tmpDir))...)
	return r, nil
}

// donorVendorBaseline unpacks the donor's own vendor (best effort) so the
// HAL audit can report regressions instead of absolute gaps. Returns "" when
// the donor ships no vendor (system-only donors, GSI-style).
func donorVendorBaseline(p *project.Project) string {
	tmpDonor := filepath.Join(p.Dir, ".deep_donor")
	_ = os.RemoveAll(tmpDonor)
	defer os.RemoveAll(tmpDonor)
	if out, err := util.RunRomtool("init", "-rom", p.DonorRoot, "-dir", tmpDonor); err != nil {
		_ = out
		debugf("baseline init failed: %v %.200s", err, out)
		return ""
	}
	if out, err := util.RunRomtool("unpack-partition", "-project", tmpDonor, "-name", "vendor"); err != nil {
		_ = out
		debugf("baseline unpack failed: %v %.200s", err, out)
		return ""
	}
	dir := filepath.Join(tmpDonor, "partitions", "vendor")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		debugf("baseline vendor dir missing: %v", err)
		return ""
	}
	// Persist beyond the deferred cleanup: copy manifests to a kept dir.
	kept := filepath.Join(p.Dir, ".deep_baseline")
	_ = os.RemoveAll(kept)
	src := filepath.Join(dir, "etc", "vintf")
	if _, err := os.Stat(src); err != nil {
		debugf("baseline vintf missing: %v", err)
		return ""
	}
	_ = os.MkdirAll(filepath.Join(kept, "etc"), 0755)
	if err := copyTree(src, filepath.Join(kept, "etc", "vintf")); err != nil {
		debugf("baseline copy failed: %v", err)
		return ""
	}
	return kept
}

func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

func targetOf(p *project.Project) string {
	for _, cand := range []string{p.Meta.Device, p.Base.Device} {
		if cand != "" && cand != "unknown" {
			if err := project.ValidateDevice(cand); err == nil {
				return cand
			}
		}
	}
	if len(p.Base.UpdaterDevices) > 0 {
		return p.Base.UpdaterDevices[0]
	}
	return p.Base.Device
}

func firstExisting(cands ...string) string {
	for _, c := range cands {
		if util.FileExists(c) {
			return c
		}
	}
	return cands[0]
}

// kernelReleaseOf sniffs major.minor from the unpacked target kernel.
func kernelReleaseOf(tmpDir string) string {
	entries, err := os.ReadDir(filepath.Join(tmpDir, "boot"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if kv := project.SniffKernelVersion(filepath.Join(tmpDir, "boot", e.Name(), "kernel")); kv != "" {
			return kv
		}
	}
	return ""
}

func shortOut(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func debugf(format string, args ...any) {
	if os.Getenv("PORTLAB_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[debug] "+format+"\n", args...)
	}
}
// Modules built for another kernel refuse to load (dead wifi/display) or,
// worse, oops on load. Target-kept vendors normally pass; user mixing fails.
// KoAudit checks kernel-module vermagic against the running kernel release.
// Modules built for another kernel refuse to load (dead wifi/display) or,
// worse, oops on load. Target-kept vendors normally pass; user mixing fails.
func KoAudit(modulesDirs []string, kernelRelease string) []Finding {
	var out []Finding
	checked := 0
	for _, dir := range modulesDirs {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".ko") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			checked++
			ver := ""
			s := string(data)
			if i := strings.Index(s, "vermagic="); i >= 0 {
				rest := s[i+len("vermagic="):]
				if j := strings.IndexAny(rest, " \x00"); j >= 0 {
					ver = rest[:j]
				}
			}
			if ver == "" {
				return nil
			}
			if kernelRelease != "" && !strings.HasPrefix(ver, kernelRelease) {
				out = append(out, Finding{"ko/"+filepath.Base(path), NoBoot, "modules",
					fmt.Sprintf("%s built for %s, kernel is %s — load will fail", filepath.Base(path), ver, kernelRelease),
					false, "use modules matching the target kernel"})
			}
			return nil
		})
	}
	if checked == 0 {
		out = append(out, Finding{"ko-none", Info, "modules", "no .ko modules found to check", true, ""})
	} else if len(out) == 0 {
		out = append(out, Finding{"ko-ok", Info, "modules", "checked module vermagic against kernel", true, ""})
	}
	return out
}
