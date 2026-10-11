// Package project owns the portlab on-disk state (port_project.json).
// Standalone: does not import romtool. Heavy unpack/repack is delegated
// to the romtool binary + toolkit binaries via exec.
package project

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"portlab/internal/dyn"
	"portlab/internal/util"
)

const ConfigFile = "port_project.json"

type Fingerprint struct {
	Label          string   `json:"label"`
	SourcePath     string   `json:"source_path"`
	Device         string   `json:"device"`
	Brand          string   `json:"brand"`
	Model          string   `json:"model"`
	Platform       string   `json:"platform"`
	Arch           string   `json:"arch"`
	ABIs           string   `json:"abis"`
	AndroidVersion string   `json:"android_version"`
	SDK            string   `json:"sdk"`
	VendorSDK      string   `json:"vendor_sdk"`
	VNDK           string   `json:"vndk"`
	Treble         string   `json:"treble"`
	SAR            string   `json:"sar"`
	ABUpdate       string   `json:"ab_update"`
	Dynamic        bool     `json:"dynamic"`
	Layout         string   `json:"layout"`
	AVB            bool     `json:"avb"`
	RomType        string   `json:"rom_type"`
	Partitions     []string `json:"partitions"`
	BootImages     []string `json:"boot_images"`
	UpdaterDevices []string `json:"updater_devices"`
	HasOpList      bool     `json:"has_oplist"`
	HasPayload     bool     `json:"has_payload"`
	HasSuper       bool     `json:"has_super"`
	KernelVersion  string   `json:"kernel_version"`
	FromUnpacked   bool     `json:"from_unpacked"`
}

type Meta struct {
	Name       string            `json:"name"`
	Device     string            `json:"device"`
	Maintainer string            `json:"maintainer"`
	Version    string            `json:"version"`
	Changelog  string            `json:"changelog"`
	Extra      map[string]string `json:"extra"`
}

type Project struct {
	Dir        string      `json:"dir"`
	DonorSrc   string      `json:"donor_src"`
	BaseSrc    string      `json:"base_src"`
	DonorRoot  string      `json:"donor_root"`
	BaseRoot   string      `json:"base_root"`
	WorkDir    string      `json:"work_dir"`
	Donor      Fingerprint `json:"donor"`
	Base       Fingerprint `json:"base"`
	Meta       Meta        `json:"meta"`
	Mode       string      `json:"mode"`
	Verdict    string      `json:"verdict"`
	Score      int         `json:"score"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
	LastReport string      `json:"last_report"`
}

func (p *Project) Save() error {
	p.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.Dir, ConfigFile), data, 0644)
}

func Load(dir string) (*Project, error) {
	abs, _ := filepath.Abs(dir)
	data, err := os.ReadFile(filepath.Join(abs, ConfigFile))
	if err != nil {
		return nil, fmt.Errorf("port project not found at %s: %w", filepath.Join(abs, ConfigFile), err)
	}
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid port project: %w", err)
	}
	p.Dir = abs
	p.DonorRoot = filepath.Join(abs, "donor", "rom_root")
	p.BaseRoot = filepath.Join(abs, "base", "rom_root")
	p.WorkDir = filepath.Join(abs, "work")
	return &p, nil
}

func Init(donorSrc, baseSrc, outDir, name string) (*Project, error) {
	abs, _ := filepath.Abs(outDir)
	dAbs, _ := filepath.Abs(donorSrc)
	bAbs, _ := filepath.Abs(baseSrc)
	// Guard: output must not live inside a directory source (self-copy explosion).
	if st, err := os.Stat(donorSrc); err == nil && st.IsDir() && isWithin(abs, dAbs) {
		return nil, fmt.Errorf("output dir %s must not be inside donor source %s", abs, dAbs)
	}
	if st, err := os.Stat(baseSrc); err == nil && st.IsDir() && isWithin(abs, bAbs) {
		return nil, fmt.Errorf("output dir %s must not be inside base source %s", abs, bAbs)
	}
	donorRoot := filepath.Join(abs, "donor", "rom_root")
	baseRoot := filepath.Join(abs, "base", "rom_root")
	workDir := filepath.Join(abs, "work")
	for _, d := range []string{donorRoot, baseRoot, workDir, filepath.Join(abs, "output")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return nil, err
		}
	}
	if err := PopulateRomRoot(donorSrc, donorRoot); err != nil {
		return nil, fmt.Errorf("donor: %w", err)
	}
	if err := PopulateRomRoot(baseSrc, baseRoot); err != nil {
		return nil, fmt.Errorf("base: %w", err)
	}
	donorFP := FingerprintFromRomRoot("donor", donorSrc, donorRoot)
	baseFP := FingerprintFromRomRoot("base", baseSrc, baseRoot)
	EnrichFromUnpacked(donorSrc, &donorFP)
	EnrichFromUnpacked(baseSrc, &baseFP)
	if name == "" {
		name = fmt.Sprintf("%s-on-%s", sanitize(donorFP.Device, "donor"), sanitize(baseFP.Device, "target"))
	}
	// Compatibility is computed by caller (compat package) to avoid import cycle.
	p := &Project{
		Dir: abs, DonorSrc: donorSrc, BaseSrc: baseSrc,
		DonorRoot: donorRoot, BaseRoot: baseRoot, WorkDir: workDir,
		Donor: donorFP, Base: baseFP,
		Meta:      Meta{Name: name, Device: baseFP.Device, Extra: map[string]string{}},
		Mode:      "undecided",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := p.Save(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Project) Refresh() error {
	p.Donor = FingerprintFromRomRoot("donor", p.DonorSrc, p.DonorRoot)
	p.Base = FingerprintFromRomRoot("base", p.BaseSrc, p.BaseRoot)
	EnrichFromUnpacked(p.DonorSrc, &p.Donor)
	EnrichFromUnpacked(p.BaseSrc, &p.Base)
	return p.Save()
}

func (p *Project) SetMetadata(kv map[string]string) error {
	if p.Meta.Extra == nil {
		p.Meta.Extra = map[string]string{}
	}
	for k, v := range kv {
		if strings.ContainsAny(v, "\n\r") {
			return fmt.Errorf("metadata %s: newlines not allowed", k)
		}
		switch strings.ToLower(k) {
		case "name":
			if err := ValidateName(v); err != nil {
				return fmt.Errorf("name: %w", err)
			}
			p.Meta.Name = v
		case "device":
			if err := ValidateDevice(v); err != nil {
				return fmt.Errorf("device: %w", err)
			}
			p.Meta.Device = v
		case "maintainer":
			if len(v) > 128 {
				return fmt.Errorf("maintainer too long (max 128)")
			}
			p.Meta.Maintainer = v
		case "version":
			if len(v) > 64 {
				return fmt.Errorf("version too long (max 64)")
			}
			p.Meta.Version = v
		case "changelog":
			if len(v) > 8192 {
				return fmt.Errorf("changelog too long (max 8192)")
			}
			p.Meta.Changelog = v
		default:
			if len(k) == 0 || len(k) > 64 || strings.ContainsAny(k, "\n\r\"'\\/") {
				return fmt.Errorf("extra key %q invalid", k)
			}
			if len(v) > 4096 {
				return fmt.Errorf("extra %s too long (max 4096)", k)
			}
			p.Meta.Extra[k] = v
		}
	}
	return p.Save()
}

// ValidateDevice allows only safe updater-script / filename values.
func ValidateDevice(s string) error {
	if len(s) == 0 || len(s) > 32 {
		return fmt.Errorf("must be 1-32 chars")
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("only [A-Za-z0-9_-] allowed (got %q)", s)
	}
	return nil
}

// ValidateName is looser (zip filename) but still path/injection safe.
func ValidateName(s string) error {
	if len(s) == 0 || len(s) > 64 {
		return fmt.Errorf("must be 1-64 chars")
	}
	if strings.ContainsAny(s, "/\\\"\n\r") || strings.Contains(s, "..") {
		return fmt.Errorf("must not contain path separators, quotes, or ..")
	}
	return nil
}

func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// --- fingerprint ---

func ParseBuildProp(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return out
}

func FingerprintFromRomRoot(label, srcPath, romRoot string) Fingerprint {
	fp := Fingerprint{Label: label, SourcePath: srcPath, Device: "unknown", Arch: "unknown"}
	entries, _ := os.ReadDir(romRoot)
	names := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			names[e.Name()] = true
		}
	}
	// Layout family via dyn (single source of truth for dynamic handling).
	switch dyn.Detect(romRoot).Layout {
	case dyn.LayoutDynamicDat:
		fp.Layout = "dynamic-dat"
		fp.Dynamic = true
	case dyn.LayoutSuper:
		fp.Layout = "super"
		fp.Dynamic = true
	case dyn.LayoutPayload:
		fp.Layout = "payload"
		fp.Dynamic = true
	case dyn.LayoutNonDynamic:
		fp.Layout = "nondynamic"
	case dyn.LayoutRaw:
		fp.Layout = "raw"
	default:
		fp.Layout = "unknown"
	}
	if names["dynamic_partitions_op_list"] || names["unsparse_super_empty.img"] || names["super.img"] {
		fp.Dynamic = true
	}
	if names["super.img"] {
		fp.HasSuper = true
	}
	if names["payload.bin"] {
		fp.HasPayload = true
		fp.Dynamic = true
	}
	if names["dynamic_partitions_op_list"] {
		fp.HasOpList = true
	}
	if names["vbmeta.img"] {
		fp.AVB = true
	}
	switch {
	case names["payload.bin"]:
		fp.RomType = "payload"
	case names["dynamic_partitions_op_list"]:
		fp.RomType = "dynamic_block_ota"
	case names["super.img"]:
		fp.RomType = "super_img"
	case names["system.new.dat.br"] || names["system.new.dat"]:
		fp.RomType = "nondynamic_block_ota"
	default:
		hasImg := false
		for n := range names {
			if strings.HasSuffix(n, ".img") {
				hasImg = true
				break
			}
		}
		if hasImg {
			fp.RomType = "raw_images"
		} else {
			fp.RomType = "unknown"
		}
	}
	for n := range names {
		if strings.HasSuffix(n, ".new.dat.br") {
			fp.Partitions = appendUnique(fp.Partitions, strings.TrimSuffix(n, ".new.dat.br"))
		} else if strings.HasSuffix(n, ".new.dat") && !strings.HasSuffix(n, ".patch.dat") {
			fp.Partitions = appendUnique(fp.Partitions, strings.TrimSuffix(n, ".new.dat"))
		} else if strings.HasSuffix(n, ".img") {
			base := strings.TrimSuffix(n, ".img")
			switch base {
			case "boot", "recovery", "vendor_boot", "init_boot", "dtbo", "vbmeta", "super_dummy", "unsparse_super_empty":
			default:
				fp.Partitions = appendUnique(fp.Partitions, base)
			}
		}
		if n == "boot.img" || n == "recovery.img" || n == "vendor_boot.img" || n == "init_boot.img" {
			fp.BootImages = appendUnique(fp.BootImages, strings.TrimSuffix(n, ".img"))
		}
	}
	if data, err := os.ReadFile(filepath.Join(romRoot, "META-INF", "com", "google", "android", "updater-script")); err == nil {
		txt := string(data)
		fp.UpdaterDevices = ParseUpdaterDevices(txt)
		if strings.Contains(strings.ToLower(txt), "slotselect") || strings.Contains(strings.ToLower(txt), "ab_update") {
			fp.ABUpdate = "true"
		}
	}
	for _, cand := range []string{filepath.Join(romRoot, "system", "build.prop")} {
		if props := ParseBuildProp(cand); len(props) > 0 {
			ApplyBuildProps(&fp, props)
			break
		}
	}
	return fp
}

// EnrichFromUnpacked merges build.prop + kernel info from an existing
// unpacked romtool project or partitions tree.
func EnrichFromUnpacked(srcPath string, fp *Fingerprint) {
	for _, c := range []string{filepath.Join(srcPath, "partitions"), srcPath} {
		merged := map[string]string{}
		found := false
		for _, part := range []string{"system", "product", "system_ext", "vendor", "odm"} {
			for _, rel := range []string{
				filepath.Join(c, part, "build.prop"),
				filepath.Join(c, part, "etc", "build.prop"),
				filepath.Join(c, part, "system", "build.prop"),
			} {
				if props := ParseBuildProp(rel); len(props) > 0 {
					for k, v := range props {
						if _, ok := merged[k]; !ok {
							merged[k] = v
						}
					}
					found = true
				}
			}
		}
		if found {
			ApplyBuildProps(fp, merged)
			fp.FromUnpacked = true
			for _, kd := range []string{filepath.Join(srcPath, "boot", "boot", "kernel")} {
				if kv := SniffKernelVersion(kd); kv != "" {
					fp.KernelVersion = kv
					break
				}
			}
			return
		}
	}
}

func ApplyBuildProps(fp *Fingerprint, m map[string]string) {
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(m[k]); v != "" {
				return v
			}
		}
		return ""
	}
	if v := get("ro.product.device", "ro.product.system.device", "ro.build.product"); v != "" {
		fp.Device = v
	}
	if v := get("ro.product.brand", "ro.product.system.brand"); v != "" {
		fp.Brand = v
	}
	if v := get("ro.product.model", "ro.product.system.model"); v != "" {
		fp.Model = v
	}
	if v := get("ro.board.platform", "ro.hardware", "ro.product.board"); v != "" {
		fp.Platform = v
	}
	if v := get("ro.product.cpu.abi", "ro.product.cpu.abilist"); v != "" {
		fp.ABIs = v
		fp.Arch = NormalizeArch(v)
	}
	if v := get("ro.bionic.arch"); v != "" && fp.Arch == "unknown" {
		fp.Arch = NormalizeArch(v)
	}
	if v := get("ro.system.build.version.sdk", "ro.build.version.sdk"); v != "" {
		fp.SDK = v
	}
	if v := get("ro.vendor.build.version.sdk"); v != "" {
		fp.VendorSDK = v
	}
	if v := get("ro.vndk.version", "ro.vendor.vndk.version"); v != "" {
		fp.VNDK = v
	}
	if v := get("ro.build.version.release", "ro.system.build.version.release"); v != "" {
		fp.AndroidVersion = v
	}
	if v := get("ro.treble.enabled"); v != "" {
		fp.Treble = v
	}
	if v := get("ro.build.system_root_image"); v != "" {
		fp.SAR = v
	}
	if v := get("ro.build.ab_update"); v != "" && fp.ABUpdate == "" {
		fp.ABUpdate = v
	}
}

func NormalizeArch(s string) string {
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "arm64") || strings.Contains(low, "aarch64"):
		return "arm64"
	case strings.Contains(low, "x86_64"):
		return "x86_64"
	case strings.Contains(low, "x86"):
		return "x86"
	case strings.Contains(low, "armeabi") || strings.Contains(low, "arm"):
		return "arm"
	default:
		return strings.TrimSpace(strings.Split(s, ",")[0])
	}
}

func ParseUpdaterDevices(script string) []string {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "getprop") {
			continue
		}
		parts := strings.Split(line, "\"")
		for i := 1; i < len(parts); i += 2 {
			cand := strings.TrimSpace(parts[i])
			if cand == "" || strings.HasPrefix(cand, "ro.") || strings.Contains(cand, " ") || strings.Contains(cand, "(") {
				continue
			}
			if len(cand) >= 2 && len(cand) <= 32 {
				out = appendUnique(out, cand)
			}
		}
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

func SniffKernelVersion(kernelPath string) string {
	data, err := os.ReadFile(kernelPath)
	if err != nil || len(data) == 0 {
		return ""
	}
	idx := strings.Index(string(data), "Linux version ")
	if idx < 0 {
		return ""
	}
	end := idx + len("Linux version ")
	for end < len(data) && end < idx+64 {
		c := data[end]
		if c == ' ' || c == '\n' || c == 0 {
			break
		}
		end++
	}
	return strings.TrimSpace(string(data[idx+len("Linux version ") : end]))
}

func appendUnique(in []string, s string) []string {
	for _, x := range in {
		if x == s {
			return in
		}
	}
	return append(in, s)
}

func sanitize(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "unknown" {
		return fallback
	}
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// PopulateRomRoot extracts zip/dir/project/payload.bin into dstRoot.
func PopulateRomRoot(src, dstRoot string) error {
	stat, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("source not accessible %s: %w", src, err)
	}
	if stat.IsDir() {
		for _, cand := range []string{
			filepath.Join(src, "rom_root"),
			filepath.Join(src, "donor", "rom_root"),
			filepath.Join(src, "base", "rom_root"),
			src,
		} {
			if st, err := os.Stat(cand); err == nil && st.IsDir() && LooksLikeRomRoot(cand) {
				if cand == dstRoot {
					return nil
				}
				return util.CopyDirNoLink(cand, dstRoot)
			}
		}
		return fmt.Errorf("directory %s does not look like a ROM or romtool project", src)
	}
	low := strings.ToLower(src)
	if strings.HasSuffix(low, ".zip") {
		return util.ExtractZipSafe(src, dstRoot)
	}
	if strings.HasSuffix(low, ".bin") {
		_ = os.MkdirAll(dstRoot, 0755)
		return util.CopyFile(src, filepath.Join(dstRoot, "payload.bin"))
	}
	return fmt.Errorf("unsupported source %s (use .zip, payload.bin, or directory)", src)
}

func LooksLikeRomRoot(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if n == "META-INF" || n == "payload.bin" || n == "dynamic_partitions_op_list" ||
			n == "super.img" || strings.HasSuffix(n, ".new.dat.br") ||
			strings.HasSuffix(n, ".new.dat") || strings.HasSuffix(n, ".img") {
			return true
		}
	}
	return false
}
