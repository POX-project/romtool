package util

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorkspaceRoot finds the workspace containing toolkit/bin + bin/romtool.
func WorkspaceRoot() string {
	exe, _ := os.Executable()
	dirs := []string{}
	if exe != "" {
		d := filepath.Dir(exe)
		dirs = append(dirs, d, filepath.Join(d, ".."), filepath.Join(d, "..", ".."))
	}
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, cwd, filepath.Join(cwd, ".."))
	}
	dirs = append(dirs, "/mnt/9f4d8733-a954-435d-b92f-f33c068c3b1e/repack bego")
	for _, d := range dirs {
		abs, _ := filepath.Abs(d)
		if _, err := os.Stat(filepath.Join(abs, "toolkit", "bin")); err == nil {
			return abs
		}
		if _, err := os.Stat(filepath.Join(abs, "bin", "romtool")); err == nil {
			return abs
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

func ToolkitBin() string { return filepath.Join(WorkspaceRoot(), "toolkit", "bin") }
func RomtoolBin() string { return filepath.Join(WorkspaceRoot(), "bin", "romtool") }

// RunTool runs a toolkit binary (falls back to PATH), niced to idle
// priority so heavy images never freeze the desktop. Matches romtool.
func RunTool(name string, args ...string) (string, error) {
	bin, err := ToolPath(name)
	if err != nil {
		return "", err
	}
	var cmd *exec.Cmd
	if _, err := exec.LookPath("nice"); err == nil {
		niceArgs := []string{"-n", "19", bin}
		if _, err := exec.LookPath("ionice"); err == nil {
			niceArgs = []string{"-n", "19", "ionice", "-c", "3", bin}
		}
		cmd = exec.Command("nice", append(niceArgs, args...)...)
	} else {
		cmd = exec.Command(bin, args...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// RunPayload runs payload-dumper-go with worker count capped by RAM/CPU
// (default 8 workers on a 15GB no-swap box risks OOM on GB payloads).
func RunPayload(args ...string) (string, error) {
	for _, a := range args {
		if a == "-c" || a == "-concurrency" || strings.HasPrefix(a, "-c=") {
			return RunTool("payload-dumper-go", args...)
		}
	}
	return RunTool("payload-dumper-go", append([]string{"-c", payloadWorkers()}, args...)...)
}

// ToolPath resolves a toolkit binary, falling back to PATH.
func ToolPath(name string) (string, error) {
	bin := filepath.Join(ToolkitBin(), name)
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("tool %q not found", name)
}

// RunRomtool runs the existing romtool binary (read-only orchestration, no modification).
func RunRomtool(args ...string) (string, error) {
	bin := RomtoolBin()
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("romtool binary not found at %s", bin)
	}
	var cmd *exec.Cmd
	if _, err := exec.LookPath("nice"); err == nil {
		niceArgs := []string{"-n", "19", bin}
		if _, err := exec.LookPath("ionice"); err == nil {
			niceArgs = []string{"-n", "19", "ionice", "-c", "3", bin}
		}
		cmd = exec.Command("nice", append(niceArgs, args...)...)
	} else {
		cmd = exec.Command(bin, args...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func CopyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, _ := in.Stat()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if st != nil {
		_ = os.Chmod(dst, st.Mode())
	}
	return nil
}

func CopyDir(src, dst string) error {
	return copyDir(src, dst, true)
}

// CopyDirNoLink always copies bytes (no hardlinks). Use it at project
// boundaries (donor/base populate, base->work clone): hardlinked work
// files would let an in-place edit corrupt the pristine source, and
// byte audits cannot see same-inode tampering.
func CopyDirNoLink(src, dst string) error {
	return copyDir(src, dst, false)
}

func copyDir(src, dst string, hardlink bool) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil
		}
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing to copy escaping path %q", rel)
		}
		target := filepath.Join(dst, rel)
		// Preserve symlinks as symlinks (never follow them).
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("failed reading symlink %s: %w", path, err)
			}
			_ = os.Remove(target)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		_ = os.Remove(target)
		if hardlink {
			if err := os.Link(path, target); err == nil {
				return nil
			}
		}
		return CopyFile(path, target)
	})
}

// ExtractZipSafe prevents ZipSlip, skips symlinks, and caps zip bombs.
func ExtractZipSafe(zipPath, dst string) error {
	const maxFiles = 300000
	const maxTotalBytes = 30 << 30 // 30 GiB uncompressed cap
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	if len(r.File) > maxFiles {
		return fmt.Errorf("zip has %d entries (limit %d) — refusing (possible zip bomb)", len(r.File), maxFiles)
	}
	var total uint64
	for _, f := range r.File {
		total += f.UncompressedSize64
		if total > maxTotalBytes {
			return fmt.Errorf("zip uncompressed size exceeds %d bytes — refusing (possible zip bomb)", maxTotalBytes)
		}
	}
	clean := filepath.Clean(dst)
	_ = os.MkdirAll(clean, 0755)
	for _, f := range r.File {
		// Never materialize symlinks from zips.
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			continue
		}
		rel := filepath.Clean(f.Name)
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		out := filepath.Join(clean, rel)
		if !strings.HasPrefix(out, clean+string(filepath.Separator)) && out != clean {
			continue
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(out, 0755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(out), 0755)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		of, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(of, rc)
		of.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// CreateZip zips srcDir into outZip (Store for images/dat/br/apk).
func CreateZip(srcDir, outZip string) error {
	_ = os.MkdirAll(filepath.Dir(outZip), 0755)
	tmp := outZip + ".tmp"
	_ = os.Remove(tmp)
	zf, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(zf)
	clean := filepath.Clean(srcDir)
	err = filepath.Walk(clean, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Never follow or archive symlinks (prevents info leak / zip bombs).
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(clean, path)
		if err != nil || rel == "." {
			return nil
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		ext := strings.ToLower(filepath.Ext(path))
		if info.IsDir() || ext == ".br" || ext == ".img" || ext == ".apk" || ext == ".dat" {
			h.Method = zip.Store
		} else {
			h.Method = zip.Deflate
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		zw.Close()
		zf.Close()
		_ = os.Remove(tmp)
		return err
	}
	zw.Close()
	zf.Close()
	return os.Rename(tmp, outZip)
}

func WriteFile(path, content string) error {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	return os.WriteFile(path, []byte(content), 0644)
}

func FileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
