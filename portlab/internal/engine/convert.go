// Conversion paths between container families.
//
// All heavy lifting is delegated to existing binaries (romtool, lpunpack,
// lpmake, payload-dumper-go, simg2img). This file only orchestrates and
// validates — it never writes an unchecked image into the output.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"portlab/internal/dyn"
	"portlab/internal/util"
)

// stageDonorRaw returns a raw .img for a donor partition, converting from
// whatever container the donor uses. Results are cached in tmpDir.
func stageDonorRaw(donorRoot, tmpDir, name string) (string, error) {
	cached := filepath.Join(tmpDir, name+".img")
	if util.FileExists(cached) {
		if _, err := dyn.ValidateImage(cached, 1<<20); err == nil {
			return cached, nil
		}
		_ = os.Remove(cached)
	}
	// 1. Direct loose image.
	if direct := filepath.Join(donorRoot, name+".img"); util.FileExists(direct) {
		if _, err := dyn.ValidateImage(direct, 1<<20); err != nil {
			return "", fmt.Errorf("donor %s: %w", name, err)
		}
		return direct, nil
	}
	// 2. Inside donor super.img.
	if util.FileExists(filepath.Join(donorRoot, "super.img")) {
		outDir := filepath.Join(tmpDir, "donor_super")
		if err := unpackSuper(donorRoot, outDir); err != nil {
			return "", fmt.Errorf("donor super unpack: %w", err)
		}
		if cand := filepath.Join(outDir, name+".img"); util.FileExists(cand) {
			if _, err := dyn.ValidateImage(cand, 1<<20); err != nil {
				return "", fmt.Errorf("donor super %s: %w", name, err)
			}
			return cand, nil
		}
		return "", fmt.Errorf("donor super.img has no %s logical partition", name)
	}
	// 3. Inside donor payload.bin.
	if util.FileExists(filepath.Join(donorRoot, "payload.bin")) {
		outDir := filepath.Join(tmpDir, "donor_payload")
		_ = os.MkdirAll(outDir, 0755)
		if _, err := util.RunPayload( "-o", outDir, "-p", name,
			filepath.Join(donorRoot, "payload.bin")); err != nil {
			return "", fmt.Errorf("payload extract %s: %w", name, err)
		}
		if cand := filepath.Join(outDir, name+".img"); util.FileExists(cand) {
			if _, err := dyn.ValidateImage(cand, 1<<20); err != nil {
				return "", fmt.Errorf("donor payload %s: %w", name, err)
			}
			return cand, nil
		}
		return "", fmt.Errorf("donor payload.bin has no %s partition", name)
	}
	// 4. dat/br via romtool unpack orchestration (handles brotli + sdat).
	if dyn.Format(donorRoot, name) == dyn.SrcDat || dyn.Format(donorRoot, name) == dyn.SrcDatBr {
		if err := dyn.ValidateDatPair(donorRoot, name); err != nil {
			return "", fmt.Errorf("donor %s: %w", name, err)
		}
		projDir := filepath.Join(tmpDir, "donor_proj")
		if !util.FileExists(filepath.Join(projDir, "rom_project.json")) {
			_ = os.RemoveAll(projDir)
			if out, err := util.RunRomtool("init", "-rom", donorRoot, "-dir", projDir); err != nil {
				return "", fmt.Errorf("romtool init donor: %v (%s)", err, short(out))
			}
		}
		if out, err := util.RunRomtool("unpack-partition", "-project", projDir, "-name", name); err != nil {
			return "", fmt.Errorf("romtool unpack %s: %v (%s)", name, err, short(out))
		}
		if cand := filepath.Join(projDir, "raw_images", name+".img"); util.FileExists(cand) {
			if _, err := dyn.ValidateImage(cand, 1<<20); err != nil {
				return "", fmt.Errorf("donor unpacked %s: %w", name, err)
			}
			return cand, nil
		}
		return "", fmt.Errorf("romtool unpacked %s but raw image missing", name)
	}
	return "", fmt.Errorf("donor has no usable %s (no dat/dat.br/img/super/payload)", name)
}

// rawToDat converts a raw .img into <name>.new.dat + transfer.list in outDir
// via romtool img2sdat, then optionally compresses to .br via romtool repack.
func rawToDat(rawImg, outDir, name string) error {
	if _, err := dyn.ValidateImage(rawImg, 1<<20); err != nil {
		return err
	}
	out, err := util.RunRomtool("img2sdat", "-img", rawImg, "-name", name, "-out", outDir)
	if err != nil {
		return fmt.Errorf("img2sdat %s: %v (%s)", name, err, short(out))
	}
	dat := filepath.Join(outDir, name+".new.dat")
	tl := filepath.Join(outDir, name+".transfer.list")
	if !util.FileExists(dat) || !util.FileExists(tl) {
		return fmt.Errorf("img2sdat %s produced no dat/transfer.list", name)
	}
	// Guard the img2sdat total bug (E1001): total must cover all ranges.
	if _, err := dyn.FixTransferList(tl); err != nil {
		return fmt.Errorf("transfer-list check %s: %w", name, err)
	}
	return nil
}

// unpackSuper extracts logical partition images from super.img via lpunpack.
// Sparse super is unsparsed first. Output: outDir/<part>.img
func unpackSuper(romRoot, outDir string) error {
	return unpackSuperFile(filepath.Join(romRoot, "super.img"), outDir)
}

func unpackSuperFile(superPath, outDir string) error {
	if err := dyn.ValidateSuper(superPath, 1<<20); err != nil {
		return fmt.Errorf("super: %w", err)
	}
	kind, kerr := dyn.ValidateImage(superPath, 0)
	target := superPath
	if kerr == nil && kind == dyn.KindSparse {
		raw := filepath.Join(outDir, ".unsparsed_super.img")
		_ = os.MkdirAll(outDir, 0755)
		if out, err := util.RunTool("simg2img", superPath, raw); err != nil {
			return fmt.Errorf("simg2img super: %v (%s)", err, short(out))
		}
		target = raw
	}
	_ = os.MkdirAll(outDir, 0755)
	out, err := util.RunTool("lpunpack", target, outDir)
	if err != nil {
		return fmt.Errorf("lpunpack: %v (%s)", err, short(out))
	}
	return nil
}

// rebuildSuper builds a new super.img from logical images.
// deviceSize = super partition budget (base super size). slots = metadata slots (1 or 2).
// Refuses when the content does not fit — an oversized super is a brick vector.
func rebuildSuper(logicalDir, outPath string, deviceSize int64, slots int, group string) error {
	entries, err := os.ReadDir(logicalDir)
	if err != nil {
		return err
	}
	type part struct {
		name string
		path string
		size int64
	}
	var parts []part
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".img") {
			continue
		}
		p := filepath.Join(logicalDir, e.Name())
		st, err := os.Stat(p)
		if err != nil || st.Size() == 0 {
			return fmt.Errorf("logical %s empty/missing — refusing super rebuild", e.Name())
		}
		if _, err := dyn.ValidateImage(p, 1024); err != nil {
			return fmt.Errorf("logical %s: %w", e.Name(), err)
		}
		name := strings.TrimSuffix(e.Name(), ".img")
		// Align each partition up like lpmake expects room for.
		size := alignUp(st.Size(), 4096)
		parts = append(parts, part{name, p, size})
		total += size
	}
	if len(parts) == 0 {
		return fmt.Errorf("no logical images in %s — refusing super rebuild", logicalDir)
	}
	const metadataOverhead = 4 << 20
	budget := deviceSize - metadataOverhead
	if budget <= 0 {
		return fmt.Errorf("invalid super budget %d", deviceSize)
	}
	if total > budget {
		return fmt.Errorf("logical content %d bytes exceeds super budget %d (over by %d) — donor too big, use GSI fallback",
			total, budget, total-budget)
	}
	if group == "" {
		group = "portlab"
	}
	args := []string{
		"--metadata-size", "65536",
		"--super-name", "super",
		"--metadata-slots", fmt.Sprintf("%d", slots),
		"--device", fmt.Sprintf("super:%d", deviceSize),
		"--group", fmt.Sprintf("%s:%d", group, budget),
	}
	for _, pt := range parts {
		args = append(args, "--partition", fmt.Sprintf("%s:readonly:%d:%s", pt.name, pt.size, group),
			"--image", fmt.Sprintf("%s=%s", pt.name, pt.path))
	}
	args = append(args, "--output", outPath)
	out, err := util.RunTool("lpmake", args...)
	if err != nil {
		return fmt.Errorf("lpmake: %v (%s)", err, short(out))
	}
	st, err := os.Stat(outPath)
	if err != nil || st.Size() == 0 {
		return fmt.Errorf("lpmake produced no super.img")
	}
	if err := dyn.ValidateSuper(outPath, 1<<20); err != nil {
		_ = os.Remove(outPath)
		return fmt.Errorf("rebuilt super failed validation: %w", err)
	}
	if st.Size() > deviceSize {
		_ = os.Remove(outPath)
		return fmt.Errorf("rebuilt super %d bytes exceeds budget %d — refusing (would brick)", st.Size(), deviceSize)
	}
	return nil
}

// payloadList returns partition names inside payload.bin.
func payloadList(romRoot string) []string {
	pb := filepath.Join(romRoot, "payload.bin")
	if !util.FileExists(pb) {
		return nil
	}
	out, err := util.RunPayload( "-list", pb)
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		for _, f := range strings.Fields(line) {
			f = strings.Trim(f, ",:\"'")
			if f == "" || strings.Contains(f, "/") || strings.Contains(f, ".") {
				continue
			}
			names = append(names, f)
		}
	}
	return names
}

// superLogicals unpacks super.img once into cacheDir and lists logicals.
func superLogicals(romRoot, cacheDir string) []string {
	if !util.FileExists(filepath.Join(romRoot, "super.img")) {
		return nil
	}
	outDir := filepath.Join(cacheDir, "super_probe")
	if _, err := os.Stat(outDir); err != nil {
		if uerr := unpackSuperFile(filepath.Join(romRoot, "super.img"), outDir); uerr != nil {
			return nil
		}
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".img") {
			names = append(names, strings.TrimSuffix(e.Name(), ".img"))
		}
	}
	return names
}

func containsName(in []string, wants ...string) bool {
	for _, x := range in {
		for _, w := range wants {
			if x == w {
				return true
			}
		}
	}
	return false
}

// vendorPresent checks loose files, super logicals, or payload listing.
func vendorPresent(root, probeDir string) bool {
	if hasVendor(root) {
		return true
	}
	if containsName(superLogicals(root, probeDir), "vendor", "odm", "vendor_dlkm", "odm_dlkm") {
		return true
	}
	return containsName(payloadList(root), "vendor", "odm", "vendor_dlkm", "odm_dlkm")
}

// payloadBoot extracts boot from a payload base into probeDir for validation.
func payloadBoot(root, probeDir string) string {
	if !containsName(payloadList(root), "boot", "init_boot") {
		return ""
	}
	name := "boot"
	if containsName(payloadList(root), "init_boot") {
		name = "init_boot"
	}
	outDir := filepath.Join(probeDir, "payload_boot")
	_ = os.MkdirAll(outDir, 0755)
	if _, err := util.RunPayload( "-o", outDir, "-p", name, filepath.Join(root, "payload.bin")); err != nil {
		return ""
	}
	if cand := filepath.Join(outDir, name+".img"); util.FileExists(cand) {
		dst := filepath.Join(probeDir, name+".img")
		_ = util.CopyFile(cand, dst)
		return dst
	}
	return ""
}

// materializeBase extracts containerized base boot/vendor into the work tree
// (payload bases) so the output is self-contained loose files. super.img
// bases keep their super (rebuilt later); boot.img ships loose there.
func materializeBase(workRoot, stageDir string) error {
	if !util.FileExists(filepath.Join(workRoot, "payload.bin")) {
		return nil
	}
	list := payloadList(workRoot)
	if len(list) == 0 {
		return fmt.Errorf("base payload.bin unreadable — refusing")
	}
	outDir := filepath.Join(stageDir, "base_payload")
	_ = os.MkdirAll(outDir, 0755)
	var want []string
	for _, n := range []string{"boot", "init_boot", "vendor", "odm", "vendor_dlkm", "odm_dlkm"} {
		if containsName(list, n) && !util.FileExists(filepath.Join(workRoot, n+".img")) {
			want = append(want, n)
		}
	}
	if len(want) > 0 {
		if _, err := util.RunPayload( "-o", outDir, "-p", strings.Join(want, ","),
			filepath.Join(workRoot, "payload.bin")); err != nil {
			return fmt.Errorf("base payload extract: %w", err)
		}
		for _, n := range want {
			cand := filepath.Join(outDir, n+".img")
			if !util.FileExists(cand) {
				return fmt.Errorf("base payload lacks %s after extract — refusing", n)
			}
			if n == "boot" || n == "init_boot" {
				if err := dyn.ValidateBootImg(cand); err != nil {
					return fmt.Errorf("base payload boot corrupt: %w", err)
				}
			}
			if err := util.CopyFile(cand, filepath.Join(workRoot, n+".img")); err != nil {
				return err
			}
		}
	}
	// payload.bin is source-only: drop it so flashers don't double-ship GBs.
	_ = os.Remove(filepath.Join(workRoot, "payload.bin"))
	return nil
}
// detectSlots guesses LP metadata slots: 2 on A/B, else 1.
func detectSlots(baseRoot string, abUpdate string) int {
	if abUpdate == "true" {
		return 2
	}
	for _, f := range []string{"vendor_boot.img", "init_boot.img"} {
		if util.FileExists(filepath.Join(baseRoot, f)) {
			return 2
		}
	}
	if data, err := os.ReadFile(filepath.Join(baseRoot, "META-INF", "com", "google", "android", "updater-script")); err == nil {
		if strings.Contains(strings.ToLower(string(data)), "slotselect") {
			return 2
		}
	}
	return 1
}

func alignUp(v, align int64) int64 {
	return ((v + align - 1) / align) * align
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}
