package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"portlab/internal/project"
)

func writeF(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func testProj(t *testing.T, donorRoot, baseRoot string) *project.Project {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work", "rom_root")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	return &project.Project{
		Dir: dir, DonorSrc: donorRoot, BaseSrc: baseRoot,
		DonorRoot: donorRoot, BaseRoot: baseRoot, WorkDir: filepath.Join(dir, "work"),
		Donor: project.Fingerprint{Device: "tetris", Arch: "arm64", Platform: "mt6877"},
		Base:  project.Fingerprint{Device: "m20lte", Arch: "arm64", Platform: "exynos7904"},
		Meta:  project.Meta{Name: "t", Device: "m20lte", Extra: map[string]string{}},
	}
}

func baseTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeF(t, filepath.Join(dir, "boot.img"), "ANDROID!basekernel.....")
	writeF(t, filepath.Join(dir, "vendor.new.dat.br"), "vendorbase")
	writeF(t, filepath.Join(dir, "vendor.transfer.list"), "t")
	writeF(t, filepath.Join(dir, "system.new.dat.br"), "systembase")
	writeF(t, filepath.Join(dir, "system.transfer.list"), "t")
	writeF(t, filepath.Join(dir, "META-INF", "com", "google", "android", "updater-script"),
		"getprop(\"ro.product.device\") == \"m20lte\" || abort(\"x\");\n")
	return dir
}

func TestContaminationClean(t *testing.T) {
	base := baseTree(t)
	p := testProj(t, t.TempDir(), base)
	// work == base clone, no report (nothing overlaid)
	copyDir(t, base, filepath.Join(p.WorkDir, "rom_root"))
	r := Audit(p)
	for _, f := range r.Blockers() {
		t.Fatalf("unexpected blocker: [%s] %s", f.Severity, f.Detail)
	}
}

func TestContaminationBootTamperIsHardBrick(t *testing.T) {
	base := baseTree(t)
	p := testProj(t, t.TempDir(), base)
	work := filepath.Join(p.WorkDir, "rom_root")
	copyDir(t, base, work)
	os.WriteFile(filepath.Join(work, "boot.img"), []byte("ANDROID!DONORMIX...."), 0644)
	r := Audit(p)
	found := false
	for _, f := range r.Findings {
		if f.Severity == HardBrick && contains(f.Detail, "boot") {
			found = true
		}
	}
	if !found {
		t.Fatalf("tampered boot must be HARD_BRICK, got %+v", r.Findings)
	}
}

func TestContaminationVendorTamperIsNoBoot(t *testing.T) {
	base := baseTree(t)
	p := testProj(t, t.TempDir(), base)
	work := filepath.Join(p.WorkDir, "rom_root")
	copyDir(t, base, work)
	os.WriteFile(filepath.Join(work, "vendor.new.dat.br"), []byte("different-size-here!!"), 0644)
	r := Audit(p)
	found := false
	for _, f := range r.Findings {
		if f.Severity == NoBoot && contains(f.ID, "contam") {
			found = true
		}
	}
	if !found {
		t.Fatalf("tampered vendor must be NO_BOOT contamination, got %+v", r.Findings)
	}
}

func TestVbmetaFlags(t *testing.T) {
	base := baseTree(t)
	p := testProj(t, t.TempDir(), base)
	work := filepath.Join(p.WorkDir, "rom_root")
	copyDir(t, base, work)
	writeF(t, filepath.Join(work, "vbmeta.img"), avbHeader(0))
	r := Audit(p)
	found := false
	for _, f := range r.Findings {
		if f.ID == "vbmeta-enabled" && f.Severity == NoBoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("enabled vbmeta must be NO_BOOT, got %+v", r.Findings)
	}
	os.WriteFile(filepath.Join(work, "vbmeta.img"), []byte(avbHeader(3)), 0644)
	r = Audit(p)
	for _, f := range r.Findings {
		if f.ID == "vbmeta-enabled" {
			t.Fatal("flags=3 must clear the finding")
		}
	}
}

func avbHeader(flags uint32) string {
	h := make([]byte, 124)
	copy(h, "AVB0")
	h[120] = byte(flags >> 24)
	h[121] = byte(flags >> 16)
	h[122] = byte(flags >> 8)
	h[123] = byte(flags)
	return string(h)
}

func TestUpdaterDonorLeak(t *testing.T) {
	base := baseTree(t)
	p := testProj(t, t.TempDir(), base)
	work := filepath.Join(p.WorkDir, "rom_root")
	copyDir(t, base, work)
	writeF(t, filepath.Join(work, "META-INF", "com", "google", "android", "updater-script"),
		"getprop(\"ro.product.device\") == \"tetris\" || abort(\"x\");\n")
	r := Audit(p)
	found := false
	for _, f := range r.Findings {
		if f.ID == "updater-donor-leak" || f.ID == "updater-assert" {
			found = true
		}
	}
	if !found {
		t.Fatalf("donor assert must be flagged, got %+v", r.Findings)
	}
}

func TestBlockersFilter(t *testing.T) {
	r := &Report{}
	r.add("x", HardBrick, "c", "d", true, "")
	r.add("y", NoBoot, "c", "d", false, "")
	if len(r.Blockers()) != 2 {
		t.Fatal("Blockers must include all severities")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			os.MkdirAll(d, 0755)
			copyDir(t, s, d)
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Dir(d), 0755)
		if err := os.WriteFile(d, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTransferSameCommands(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.list")
	b := filepath.Join(dir, "b.list")
	body := "4\nPLACEHOLDER\n0\n0\nerase 2,0,500\nnew 2,0,60\n"
	os.WriteFile(a, []byte(strings.Replace(body, "PLACEHOLDER", "100", 1)), 0644)
	os.WriteFile(b, []byte(strings.Replace(body, "PLACEHOLDER", "500", 1)), 0644)
	if !transferSameCommands(a, b) {
		t.Fatal("total-only difference must be tolerated")
	}
	os.WriteFile(b, []byte(strings.Replace(body, "PLACEHOLDER", "500", 1)+"new 2,60,70\n"), 0644)
	if transferSameCommands(a, b) {
		t.Fatal("command differences must still flag")
	}
}
