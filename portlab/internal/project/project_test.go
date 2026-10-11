package project

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateDevice(t *testing.T) {
	for _, ok := range []string{"m20lte", "a52q", "tetris_1", "X"} {
		if err := ValidateDevice(ok); err != nil {
			t.Errorf("%s should pass: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a/b", `x" || abort("`, "a b", "a;b", "ünicode-device-name-that-is-way-too-long"} {
		if err := ValidateDevice(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestSetMetadataValidation(t *testing.T) {
	dir := t.TempDir()
	writeProp(t, filepath.Join(dir, "donor", "x"), "x")
	p := &Project{Dir: dir, Meta: Meta{Extra: map[string]string{}}}
	if err := p.SetMetadata(map[string]string{"device": `x";evil`}); err == nil {
		t.Fatal("injection device must be refused")
	}
	if err := p.SetMetadata(map[string]string{"device": "m20lte"}); err != nil {
		t.Fatal(err)
	}
}

func TestInitRefusesOutputInsideSource(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "META-INF"), 0755)
	os.WriteFile(filepath.Join(src, "system.new.dat"), []byte("x"), 0644)
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "META-INF"), 0755)
	os.WriteFile(filepath.Join(base, "system.new.dat"), []byte("x"), 0644)
	if _, err := Init(src, base, filepath.Join(src, "port1"), ""); err == nil {
		t.Fatal("output inside donor source must be refused")
	}
}

func TestPopulateRomRootZipSlip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "evil.zip")
	zf, _ := os.Create(zipPath)
	zw := zip.NewWriter(zf)
	w, _ := zw.Create("../../slipped.txt")
	w.Write([]byte("evil"))
	zw.Close()
	zf.Close()
	dst := filepath.Join(dir, "root")
	if err := PopulateRomRoot(zipPath, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "slipped.txt")); err == nil {
		t.Fatal("zip traversal escaped")
	}
}

func TestFingerprintLayout(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "dynamic_partitions_op_list"), []byte("add_group g 100\n"), 0644)
	fp := FingerprintFromRomRoot("donor", dir, dir)
	if fp.Layout != "dynamic-dat" || !fp.Dynamic {
		t.Fatalf("want dynamic-dat: %+v", fp)
	}
}

func writeProp(t *testing.T, path, _ string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0755)
	os.WriteFile(path, []byte("x"), 0644)
}
