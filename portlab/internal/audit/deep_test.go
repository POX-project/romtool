package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVintfGapGrading(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "system")
	ven := filepath.Join(dir, "vendor")
	os.MkdirAll(filepath.Join(sys, "etc", "vintf"), 0755)
	os.MkdirAll(filepath.Join(ven, "etc", "vintf"), 0755)
	writeF(t, filepath.Join(sys, "etc", "vintf", "compatibility_matrix.7.xml"), `<compatibility-matrix version="7.0" type="framework">
  <hal format="hidl"><name>android.hardware.graphics.composer</name><version>2.1</version><version>2.2</version></hal>
  <hal format="hidl" optional="true"><name>android.hardware.camera.provider</name><version>2.5</version></hal>
  <hal format="aidl"><name>android.hardware.audio.effect</name><version>1</version></hal>
</compatibility-matrix>`)
	writeF(t, filepath.Join(ven, "etc", "vintf", "manifest.xml"), `<manifest version="1.0" type="device">
  <hal format="hidl"><name>android.hardware.graphics.composer</name><version>2.1</version></hal>
  <hal format="aidl"><name>android.hardware.audio.effect</name><version>1</version></hal>
</manifest>`)
	got := VintfAudit(sys, ven, "", true)
	byID := map[string]Finding{}
	for _, f := range got {
		byID[f.ID] = f
	}
	if _, ok := byID["hal/android.hardware.graphics.composer"]; ok {
		t.Fatalf("composer 2.1 satisfies 2.1|2.2 alternatives, got %+v", got)
	}
	if f, ok := byID["hal/android.hardware.camera.provider"]; !ok || f.Severity != Degraded {
		t.Fatalf("optional camera gap must be DEGRADED, got %+v", got)
	}
	if _, ok := byID["hal/android.hardware.audio.effect"]; ok {
		t.Fatalf("satisfied audio HAL must not gap, got %+v", got)
	}
}

func TestVintfTrueGapStillFlags(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "system")
	ven := filepath.Join(dir, "vendor")
	os.MkdirAll(filepath.Join(sys, "etc", "vintf"), 0755)
	os.MkdirAll(filepath.Join(ven, "etc", "vintf"), 0755)
	writeF(t, filepath.Join(sys, "etc", "vintf", "compatibility_matrix.7.xml"),
		`<compatibility-matrix version="7.0" type="framework"><hal format="hidl"><name>android.hardware.graphics.composer</name><version>2.2</version></hal></compatibility-matrix>`)
	writeF(t, filepath.Join(ven, "etc", "vintf", "manifest.xml"),
		`<manifest version="1.0" type="device"><hal format="hidl"><name>android.hardware.graphics.composer</name><version>2.1</version></hal></manifest>`)
	got := VintfAudit(sys, ven, "", true)
	found := false
	for _, f := range got {
		if f.ID == "hal/android.hardware.graphics.composer" && f.Severity == NoBoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("true composer gap must be NO_BOOT, got %+v", got)
	}
}

func TestVintfBaselineFiltersBenign(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "system")
	ven := filepath.Join(dir, "vendor")
	dven := filepath.Join(dir, "donor-vendor")
	for _, d := range []string{sys, ven, dven} {
		os.MkdirAll(filepath.Join(d, "etc", "vintf"), 0755)
	}
	matrix := `<compatibility-matrix version="7.0" type="framework">
<hal format="hidl"><name>android.hardware.tv.cec</name><version>1.0</version></hal>
<hal format="hidl"><name>android.hardware.graphics.composer</name><version>2.2</version></hal>
</compatibility-matrix>`
	writeF(t, filepath.Join(sys, "etc", "vintf", "compatibility_matrix.7.xml"), matrix)
	writeF(t, filepath.Join(ven, "etc", "vintf", "manifest.xml"),
		`<manifest version="1.0" type="device"><hal format="hidl"><name>android.hardware.graphics.composer</name><version>2.2</version></hal></manifest>`)
	writeF(t, filepath.Join(dven, "etc", "vintf", "manifest.xml"),
		`<manifest version="1.0" type="device"><hal format="hidl"><name>android.hardware.graphics.composer</name><version>2.2</version></hal></manifest>`)
	got := VintfAudit(sys, ven, dven, true)
	for _, f := range got {
		if f.ID == "hal/android.hardware.tv.cec" {
			t.Fatalf("stock-missing tv HAL is benign, got %+v", got)
		}
	}
}

func TestVintfAllSatisfied(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "system")
	ven := filepath.Join(dir, "vendor")
	os.MkdirAll(filepath.Join(sys, "etc", "vintf"), 0755)
	os.MkdirAll(filepath.Join(ven, "etc", "vintf"), 0755)
	writeF(t, filepath.Join(sys, "etc", "vintf", "compatibility_matrix.7.xml"),
		`<compatibility-matrix version="7.0" type="framework"><hal format="aidl"><name>android.hardware.audio.effect</name><version>1</version></hal></compatibility-matrix>`)
	writeF(t, filepath.Join(ven, "etc", "vintf", "manifest.xml"),
		`<manifest version="1.0" type="device"><hal format="aidl"><name>android.hardware.audio.effect</name><version>2</version></hal></manifest>`)
	got := VintfAudit(sys, ven, "", true)
	for _, f := range got {
		if f.ID != "vintf-ok" {
			t.Fatalf("want only vintf-ok, got %+v", got)
		}
	}
}

func TestVersionLess(t *testing.T) {
	if !versionLess("2.1", "2.2") || versionLess("2.2", "2.1") || !versionLess("1", "2.1") {
		t.Fatal("bad version compare")
	}
	if minRequired("2.1-2.3") != "2.1" {
		t.Fatal("bad range floor")
	}
}

func TestFstabAudit(t *testing.T) {
	dir := t.TempDir()
	writeF(t, filepath.Join(dir, "fstab.exynos"),
		"/dev/block/by-name/system /system ext4 ro 0 0\n/dev/block/by-name/vendor /vendor ext4 ro 0 0\n/dev/block/by-name/odm /odm ext4 ro 0 0\n")
	have := func(n string) bool { return n == "system" || n == "vendor" }
	got := FstabAudit([]string{dir}, have)
	byID := map[string]Finding{}
	for _, f := range got {
		byID[f.ID] = f
	}
	if byID["fstab/system"].Severity != Info || byID["fstab/odm"].Severity != NoBoot {
		t.Fatalf("bad fstab grading: %+v", got)
	}
}

func TestPropsAudit(t *testing.T) {
	got := PropsAudit(map[string]string{"ro.product.device": "tetris"}, map[string]string{}, "m20lte")
	found := false
	for _, f := range got {
		if f.ID == "props-device" && f.Severity == NoBoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("device mismatch must be NO_BOOT: %+v", got)
	}
	got = PropsAudit(map[string]string{"ro.product.device": "m20lte"}, map[string]string{}, "m20lte")
	for _, f := range got {
		if f.ID == "props-device" {
			t.Fatal("spoofed device must pass")
		}
	}
}

func TestKoAudit(t *testing.T) {
	dir := t.TempDir()
	bad := make([]byte, 64)
	copy(bad, "vermagic=4.19.100-gabcdef SMP mod_unload aarch64\x00rest")
	os.WriteFile(filepath.Join(dir, "wlan.ko"), bad, 0644)
	good := make([]byte, 64)
	copy(good, "vermagic=5.4.61-gxyz SMP mod_unload aarch64\x00rest")
	os.WriteFile(filepath.Join(dir, "gpu.ko"), good, 0644)
	got := KoAudit([]string{dir}, "5.4")
	if len(got) != 1 || got[0].Severity != NoBoot {
		t.Fatalf("want one NO_BOOT ko gap, got %+v", got)
	}
}

func TestContextsAudit(t *testing.T) {
	dir := t.TempDir()
	got := ContextsAudit(filepath.Join(dir, "s"), filepath.Join(dir, "v"))
	for _, f := range got {
		if f.ID == "selinux-plat" && f.Severity != NoBoot {
			t.Fatalf("missing plat contexts must be NO_BOOT: %+v", got)
		}
	}
}
