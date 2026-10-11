package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"portlab/internal/dyn"
	"portlab/internal/project"
	"portlab/internal/util"
)

// fixtureROM creates a rom_root with valid boot magic + vendor + system.
func fixtureROM(t *testing.T, dynamic bool) string {
	t.Helper()
	dir := t.TempDir()
	writeF(t, filepath.Join(dir, "boot.img"), "ANDROID!fake-kernel.....")
	writeF(t, filepath.Join(dir, "META-INF", "com", "google", "android", "updater-script"),
		"getprop(\"ro.product.device\") == \"m20lte\" || abort(\"wrong device\");\nshow_progress(1.0);\n")
	writeF(t, filepath.Join(dir, "vendor.new.dat.br"), "vendordata123")
	writeF(t, filepath.Join(dir, "vendor.transfer.list"), "transfer\n")
	writeF(t, filepath.Join(dir, "system.new.dat.br"), "systemdata123")
	writeF(t, filepath.Join(dir, "system.transfer.list"), "transfer\n")
	if dynamic {
		writeF(t, filepath.Join(dir, "dynamic_partitions_op_list"),
			"add_group qti_dynamic_partitions 8589934592\nadd system qti_dynamic_partitions\nadd vendor qti_dynamic_partitions\nresize system 3221225472\nresize vendor 1073741824\n")
	}
	return dir
}

func writeF(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func testProject(t *testing.T, donorRoot, baseRoot string) *project.Project {
	t.Helper()
	return &project.Project{
		Dir:       t.TempDir(),
		DonorRoot: donorRoot,
		BaseRoot:  baseRoot,
		WorkDir:   "",
		Donor:     project.Fingerprint{Device: "tetris", Arch: "arm64", SDK: "34", VNDK: "34", Treble: "true", SAR: "true", ABUpdate: "true", RomType: "dynamic_block_ota", Partitions: []string{"system"}, BootImages: []string{"boot"}},
		Base:      project.Fingerprint{Device: "m20lte", Arch: "arm64", SDK: "34", VNDK: "34", Treble: "true", SAR: "true", ABUpdate: "true", RomType: "dynamic_block_ota", Partitions: []string{"system", "vendor"}, BootImages: []string{"boot"}},
		Meta:      project.Meta{Name: "test-port", Device: "m20lte", Extra: map[string]string{}},
	}
}

func TestMakeRefusesArchMismatchEvenForced(t *testing.T) {
	donor := fixtureROM(t, false)
	base := fixtureROM(t, false)
	p := testProject(t, donor, base)
	p.WorkDir = filepath.Join(p.Dir, "work")
	p.Donor.Arch = "x86_64"
	err := Make(p, Options{Mode: "direct", Force: true})
	if err == nil || !strings.Contains(err.Error(), "ARCH MISMATCH") {
		t.Fatalf("want ARCH MISMATCH refusal, got %v", err)
	}
}

func TestMakeRefusesMissingBoot(t *testing.T) {
	donor := fixtureROM(t, false)
	base := fixtureROM(t, false)
	os.Remove(filepath.Join(base, "boot.img"))
	p := testProject(t, donor, base)
	p.WorkDir = filepath.Join(p.Dir, "work")
	if err := Make(p, Options{Mode: "direct", Force: true}); err == nil {
		t.Fatal("missing target boot must be refused")
	}
}

func TestMakeRefusesMissingVendor(t *testing.T) {
	donor := fixtureROM(t, false)
	base := fixtureROM(t, false)
	os.Remove(filepath.Join(base, "vendor.new.dat.br"))
	os.Remove(filepath.Join(base, "vendor.transfer.list"))
	p := testProject(t, donor, base)
	p.WorkDir = filepath.Join(p.Dir, "work")
	if err := Make(p, Options{Mode: "direct", Force: true}); err == nil {
		t.Fatal("missing target vendor must be refused")
	}
}

func TestMakeRefusesUnknownDevice(t *testing.T) {
	donor := fixtureROM(t, false)
	base := fixtureROM(t, false)
	p := testProject(t, donor, base)
	p.WorkDir = filepath.Join(p.Dir, "work")
	p.Base.Device = "unknown"
	p.Meta.Device = ""
	p.Base.UpdaterDevices = nil
	os.Remove(filepath.Join(base, "META-INF", "com", "google", "android", "updater-script"))
	// recreate updater without device assert parseable? remove entirely -> UpdaterDevices empty
	if err := Make(p, Options{Mode: "direct", Force: true}); err == nil {
		t.Fatal("unknown target device must be refused")
	}
}

func TestMakeRefusesEmptyDonor(t *testing.T) {
	donor := t.TempDir() // empty: no system source
	base := fixtureROM(t, false)
	p := testProject(t, donor, base)
	p.WorkDir = filepath.Join(p.Dir, "work")
	if err := Make(p, Options{Mode: "direct", Force: true}); err == nil {
		t.Fatal("empty donor must be refused, not base-only output")
	}
}

func TestMakeDatToDatKeepsTargetBoot(t *testing.T) {
	donor := fixtureROM(t, false)
	base := fixtureROM(t, false)
	// Mark donor boot distinctly to prove it never leaks.
	writeF(t, filepath.Join(donor, "boot.img"), "ANDROID!DONOR-KERNEL..")
	p := testProject(t, donor, base)
	p.WorkDir = filepath.Join(p.Dir, "work")
	if err := Make(p, Options{Mode: "direct"}); err != nil {
		t.Fatalf("make failed: %v", err)
	}
	workBoot, _ := os.ReadFile(filepath.Join(p.WorkDir, "rom_root", "boot.img"))
	if strings.Contains(string(workBoot), "DONOR") {
		t.Fatal("donor kernel leaked into output — brick vector!")
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "port_report.json")); err != nil {
		t.Fatal("report missing")
	}
	// Boot checks must have no FAIL.
	for _, c := range BootChecks(p) {
		if c.Status == "FAIL" {
			t.Fatalf("boot check FAIL: %s: %s", c.Name, c.Detail)
		}
	}
}

func TestMakeDynamicBaseUpdatesOpList(t *testing.T) {
	donor := fixtureROM(t, true)
	base := fixtureROM(t, true)
	p := testProject(t, donor, base)
	p.WorkDir = filepath.Join(p.Dir, "work")
	if err := Make(p, Options{Mode: "direct"}); err != nil {
		t.Fatalf("make failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(p.WorkDir, "rom_root", "dynamic_partitions_op_list"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "resize system") || !strings.Contains(string(data), "resize vendor") {
		t.Fatalf("op_list lost resizes:\n%s", data)
	}
}

func TestMakeDynamicToNonDynamic(t *testing.T) {
	donor := fixtureROM(t, true) // dynamic donor files are still dat/br
	base := fixtureROM(t, false)
	p := testProject(t, donor, base)
	p.WorkDir = filepath.Join(p.Dir, "work")
	if err := Make(p, Options{Mode: "direct"}); err != nil {
		t.Fatalf("dynamic->nondynamic failed: %v", err)
	}
	if !fileExists(filepath.Join(p.WorkDir, "rom_root", "system.new.dat.br")) {
		t.Fatal("donor system missing from output")
	}
	if fileExists(filepath.Join(p.WorkDir, "rom_root", "dynamic_partitions_op_list")) {
		t.Fatal("donor op_list leaked into non-dynamic output")
	}
}

func TestRewriteUpdaterRefusesInjection(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "updater-script")
	writeF(t, script, "getprop(\"ro.product.device\") == \"x\" || abort(\"no\");\n")
	if err := RewriteUpdater(script, `m20lte" || run_program("/tmp/evil") || abort("`); err == nil {
		t.Fatal("script injection must be refused")
	}
	if err := RewriteUpdater(script, "m20lte"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(script)
	if !strings.Contains(string(data), "m20lte") {
		t.Fatal("assert not rewritten")
	}
}

func TestValidateOutputZip(t *testing.T) {
	dir := t.TempDir()
	// Build a minimal valid zip via Make+manual zip? Use CreateZip on fixture.
	work := fixtureROM(t, false)
	zipPath := filepath.Join(dir, "good.zip")
	if err := util.CreateZip(work, zipPath); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOutputZip(zipPath, "m20lte"); err != nil {
		t.Fatalf("good zip refused: %v", err)
	}
	if err := ValidateOutputZip(zipPath, "otherdevice"); err == nil {
		t.Fatal("wrong-device assert must fail validation")
	}
	noboot := filepath.Join(dir, "nobot.zip")
	_ = os.Remove(filepath.Join(work, "boot.img"))
	if err := util.CreateZip(work, noboot); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOutputZip(noboot, "m20lte"); err == nil {
		t.Fatal("bootless zip must fail validation")
	}
}

func TestRebuildSuperRefusesOverflow(t *testing.T) {
	dir := t.TempDir()
	logical := filepath.Join(dir, "logical")
	os.MkdirAll(logical, 0755)
	// 2 x 3MB logicals, budget 5MB -> must refuse before calling lpmake.
	writeRaw(t, filepath.Join(logical, "system.img"), 3<<20)
	writeRaw(t, filepath.Join(logical, "vendor.img"), 3<<20)
	err := rebuildSuper(logical, filepath.Join(dir, "super.img"), 5<<20, 1, "portlab")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want budget refusal, got %v", err)
	}
	if fileExists(filepath.Join(dir, "super.img")) {
		t.Fatal("refused rebuild must not leave output")
	}
}

func TestSlottedVariants(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"system_a.img", "system_b.img", "vendor.img"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0644)
	}
	got := slottedVariants(dir, "system")
	if len(got) != 2 {
		t.Fatalf("want both slot variants, got %v", got)
	}
	if got := slottedVariants(dir, "vendor"); len(got) != 1 || got[0] != "vendor.img" {
		t.Fatalf("want plain vendor, got %v", got)
	}
	if got := slottedVariants(dir, "product"); len(got) != 0 {
		t.Fatalf("want none, got %v", got)
	}
}

func TestBaseSlotWarning(t *testing.T) {
	donor := fixtureROM(t, false)
	base := fixtureROM(t, false)
	// Donor gains product; nondynamic base has no product slot.
	writeF(t, filepath.Join(donor, "product.new.dat.br"), "p")
	writeF(t, filepath.Join(donor, "product.transfer.list"), "t")
	p := testProject(t, donor, base)
	p.WorkDir = filepath.Join(p.Dir, "work")
	if err := Make(p, Options{Mode: "direct"}); err != nil {
		t.Fatalf("make failed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(p.Dir, "port_report.json"))
	if !strings.Contains(string(data), "no product slot") {
		t.Fatalf("want base-slot warning in report: %s", data)
	}
	if fileExists(filepath.Join(p.WorkDir, "rom_root", "product.new.dat.br")) {
		t.Fatal("unflashable product must not ship in output")
	}
}

func TestApplyRefinePatches(t *testing.T) {
	tmp := t.TempDir()
	sysProp := filepath.Join(tmp, "partitions", "system", "build.prop")
	venProp := filepath.Join(tmp, "partitions", "vendor", "build.prop")
	writeF(t, sysProp, "ro.product.device=tetris\nro.build.product=tetris\n")
	writeF(t, venProp, "ro.product.device=m20lte\n")
	notes := applyRefinePatches(tmp, "m20lte", "direct", false, []string{"system", "vendor"})
	if len(notes) == 0 {
		t.Fatal("want notes")
	}
	data, _ := os.ReadFile(sysProp)
	if !strings.Contains(string(data), "ro.product.device=m20lte") {
		t.Fatalf("system not spoofed:\n%s", data)
	}
	vdata, _ := os.ReadFile(venProp)
	if !strings.Contains(string(vdata), "ro.product.device=m20lte") {
		t.Fatal("vendor must stay target-owned")
	}
	if strings.Contains(string(vdata), "tetris") {
		t.Fatal("vendor must never take donor values")
	}
}

func TestRefineFailureLeavesWorkUntouched(t *testing.T) {
	donor := fixtureROM(t, false) // fake text dat: romtool unpack fails
	base := fixtureROM(t, false)
	p := testProject(t, donor, base)
	p.WorkDir = filepath.Join(p.Dir, "work")
	if err := Make(p, Options{Mode: "direct"}); err != nil {
		t.Fatalf("make failed: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(p.WorkDir, "rom_root", "system.new.dat.br"))
	if _, err := Refine(p, false); err == nil {
		t.Fatal("refine on fake images should fail")
	}
	after, _ := os.ReadFile(filepath.Join(p.WorkDir, "rom_root", "system.new.dat.br"))
	if string(before) != string(after) {
		t.Fatal("failed refine must leave work/ untouched")
	}
}

func TestParseMissingContext(t *testing.T) {
	out := `[system] Repacking directory /tmp/x/partitions/system to system.img (ext4)...
Error: failed repacking filesystem for system: failed repacking ext4: failed running make_ext4fs [-l 3758096384 -L system -a / -S /tmp/x/partitions/system/system/etc/selinux/plat_file_contexts /tmp/x/raw_images/system.img /tmp/x/partitions/system]: exit status 1 (output: error: build_directory_structure: cannot lookup security context for /cust`
	mp, cf, part, ok := parseMissingContext(out)
	if !ok || mp != "/cust" || part != "system" {
		t.Fatalf("bad parse: %q %q %q %v", mp, cf, part, ok)
	}
	if !strings.HasSuffix(cf, "plat_file_contexts") {
		t.Fatalf("bad ctx file: %q", cf)
	}
	if _, _, _, ok := parseMissingContext("some other error"); ok {
		t.Fatal("must not parse unrelated errors")
	}
}

func TestAppendFallbackContext(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "plat_file_contexts")
	writeF(t, cf, "/system/bin/sh u:object_r:shell_exec:s0\n")
	if err := appendFallbackContext(cf, "/cust", "system"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cf)
	if !strings.Contains(string(data), "/cust(/.*)? u:object_r:system_file:s0") {
		t.Fatalf("fallback missing:\n%s", data)
	}
	if err := appendFallbackContext(cf, "/cust/app", "vendor"); err == nil {
		t.Fatal("duplicate coverage must error, not stack labels")
	}
	if err := appendFallbackContext(cf, "/x", "vendor"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(cf)
	if !strings.Contains(string(data), "vendor_file") {
		t.Fatal("vendor label wrong")
	}
}

func TestNeutralizeBoringsslReboot(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, "init.rc")
	writeF(t, rc, "service boringssl_self_test64 /system/bin/boringssl_self_test64\n    setenv BORINGSSL_SELF_TEST_CREATE_FLAG true\n    reboot_on_failure reboot,boringssl-self-check-failed\n    stdio_to_kmsg\n")
	var notes []string
	neutralizeBoringsslReboot(dir, &notes)
	data, _ := os.ReadFile(rc)
	if strings.Contains(string(data), "\n    reboot_on_failure") || strings.Contains(string(data), "    reboot_on_failure reboot") && !strings.Contains(string(data), "# portlab") {
		t.Fatalf("directive not neutered:\n%s", data)
	}
	if !strings.Contains(string(data), "boringssl_self_test64 /system/bin") {
		t.Fatal("service itself must remain (only the reboot is removed)")
	}
	if len(notes) == 0 {
		t.Fatal("want transparency note")
	}
	// Idempotent: second run changes nothing.
	before := string(data)
	neutralizeBoringsslReboot(dir, &notes)
	after, _ := os.ReadFile(rc)
	if string(after) != before {
		t.Fatal("second run must be a no-op")
	}
}

func TestStageDonorRawMissing(t *testing.T) {
	if _, err := stageDonorRaw(t.TempDir(), t.TempDir(), "system"); err == nil {
		t.Fatal("missing donor source must error")
	}
}

// TestSuperRoundtrip exercises the real lpmake/lpunpack path with tiny
// images. Skipped when toolkit binaries are absent.
func TestSuperRoundtrip(t *testing.T) {
	for _, tool := range []string{"make_ext4fs", "lpmake", "lpunpack"} {
		if _, err := toolPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
	dir := t.TempDir()
	srcSys := filepath.Join(dir, "src", "system")
	srcVen := filepath.Join(dir, "src", "vendor")
	os.MkdirAll(srcSys, 0755)
	os.MkdirAll(srcVen, 0755)
	os.WriteFile(filepath.Join(srcSys, "f.txt"), []byte("sys"), 0644)
	os.WriteFile(filepath.Join(srcVen, "f.txt"), []byte("ven"), 0644)
	sysImg, venImg := filepath.Join(dir, "system.img"), filepath.Join(dir, "vendor.img")
	if _, err := runTool("make_ext4fs", "-l", "16M", "-L", "system", "-a", "/system", sysImg, srcSys); err != nil {
		t.Skipf("make_ext4fs failed: %v", err)
	}
	if _, err := runTool("make_ext4fs", "-l", "8M", "-L", "vendor", "-a", "/vendor", venImg, srcVen); err != nil {
		t.Fatalf("make_ext4fs vendor: %v", err)
	}
	logical := filepath.Join(dir, "logical")
	os.MkdirAll(logical, 0755)
	copyF(t, sysImg, filepath.Join(logical, "system.img"))
	copyF(t, venImg, filepath.Join(logical, "vendor.img"))
	super := filepath.Join(dir, "super.img")
	if err := rebuildSuper(logical, super, 32<<20, 1, "portlab"); err != nil {
		t.Fatalf("rebuildSuper: %v", err)
	}
	outDir := filepath.Join(dir, "out")
	if err := unpackSuperFile(super, outDir); err != nil {
		t.Fatalf("unpackSuperFile: %v", err)
	}
	for _, n := range []string{"system.img", "vendor.img"} {
		if _, err := dynValidate(filepath.Join(outDir, n)); err != nil {
			t.Fatalf("%s invalid after roundtrip: %v", n, err)
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func toolPath(name string) (string, error) {
	return util.ToolPath(name)
}

func runTool(name string, args ...string) (string, error) {
	return util.RunTool(name, args...)
}

func copyF(t *testing.T, src, dst string) {
	t.Helper()
	if err := util.CopyFile(src, dst); err != nil {
		t.Fatal(err)
	}
}

func dynValidate(p string) (dyn.ImageKind, error) {
	return dyn.ValidateImage(p, 1024)
}

func writeRaw(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 1100)
	hdr[1080], hdr[1081] = 0x53, 0xef // ext4 magic
	if _, err := f.WriteAt(hdr, 0); err != nil {
		t.Fatal(err)
	}
}

var _ = dyn.LayoutDynamicDat
