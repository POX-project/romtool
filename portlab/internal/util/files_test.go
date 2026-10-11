package util

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractZipSafeSkipsTraversal(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "evil.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for _, name := range []string{"../../evil.txt", "/abs.txt", "ok/system.new.dat"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("x"))
	}
	zw.Close()
	zf.Close()
	out := filepath.Join(dir, "out")
	if err := ExtractZipSafe(zipPath, out); err != nil {
		t.Fatal(err)
	}
	if FileExists(filepath.Join(dir, "evil.txt")) || FileExists("/abs.txt") {
		t.Fatal("traversal file escaped")
	}
	if !FileExists(filepath.Join(out, "ok", "system.new.dat")) {
		t.Fatal("legit file missing")
	}
}

func TestExtractZipSafeSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "link.zip")
	zf, _ := os.Create(zipPath)
	zw := zip.NewWriter(zf)
	h := &zip.FileHeader{Name: "evil-link", Method: zip.Store}
	h.SetMode(0777 | os.ModeSymlink)
	w, _ := zw.CreateHeader(h)
	w.Write([]byte("/etc/passwd"))
	zw.Close()
	zf.Close()
	out := filepath.Join(dir, "out")
	if err := ExtractZipSafe(zipPath, out); err != nil {
		t.Fatal(err)
	}
	if FileExists(filepath.Join(out, "evil-link")) {
		t.Fatal("symlink must not be materialized")
	}
}

func TestCopyDirPreservesSymlink(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	os.WriteFile(filepath.Join(src, "real.txt"), []byte("data"), 0644)
	if err := os.Symlink("real.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Skip("symlinks unsupported here")
	}
	if err := CopyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "link.txt"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink not preserved as symlink")
	}
}

func TestCreateZipSkipsSymlinks(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("hi"), 0644)
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "leak")); err != nil {
		t.Skip("symlinks unsupported here")
	}
	out := filepath.Join(t.TempDir(), "o.zip")
	if err := CreateZip(src, out); err != nil {
		t.Fatal(err)
	}
	r, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, f := range r.File {
		if f.Name == "leak" {
			t.Fatal("symlink must not be archived")
		}
	}
}
