package partition

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"romtool/pkg/toolkit"
)

const (
	FsTypeErofs   = "erofs"
	FsTypeExt4    = "ext4"
	FsTypeSparse  = "sparse"
	FsTypeUnknown = "unknown"

	MagicSparse = 0xed26ff3a
	MagicErofs  = 0xe0f5e1e2
	MagicExt4   = 0xef53
)

// DetectFsType reads header magic bytes to detect filesystem type.
func DetectFsType(imgPath string) (string, error) {
	file, err := os.Open(imgPath)
	if err != nil {
		return FsTypeUnknown, fmt.Errorf("failed opening image %s: %w", imgPath, err)
	}
	defer file.Close()

	header := make([]byte, 2048)
	n, err := io.ReadFull(file, header)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return FsTypeUnknown, fmt.Errorf("failed reading header: %w", err)
	}
	if n < 1084 {
		return FsTypeUnknown, nil
	}

	// 1. Check Android Sparse Image (magic at 0: 0xed26ff3a)
	sparseMagic := binary.LittleEndian.Uint32(header[0:4])
	if sparseMagic == MagicSparse {
		return FsTypeSparse, nil
	}

	// 2. Check EROFS (magic at offset 1024: 0xe0f5e1e2)
	erofsMagic := binary.LittleEndian.Uint32(header[1024:1028])
	if erofsMagic == MagicErofs {
		return FsTypeErofs, nil
	}

	// 3. Check EXT4 (magic at offset 1080: 0xef53)
	ext4Magic := binary.LittleEndian.Uint16(header[1080:1082])
	if ext4Magic == MagicExt4 {
		return FsTypeExt4, nil
	}

	return FsTypeUnknown, nil
}

// ExtractFS extracts a filesystem image into targetDir.
func ExtractFS(tk *toolkit.Toolkit, imgPath, targetDir string) (string, error) {
	fsType, err := DetectFsType(imgPath)
	if err != nil {
		return "", err
	}

	// If sparse image, unsparse first to a temporary file
	if fsType == FsTypeSparse {
		unsparseImg := imgPath + ".raw"
		defer os.Remove(unsparseImg)

		_, err := tk.Run("simg2img", imgPath, unsparseImg)
		if err != nil {
			return "", fmt.Errorf("failed unsparsing image: %w", err)
		}
		imgPath = unsparseImg
		fsType, err = DetectFsType(imgPath)
		if err != nil {
			return "", err
		}
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", fmt.Errorf("failed creating target dir %s: %w", targetDir, err)
	}

	switch fsType {
	case FsTypeErofs:
		// Use fsck.erofs --extract=<targetDir> --overwrite <imgPath>
		argExtract := fmt.Sprintf("--extract=%s", targetDir)
		output, err := tk.Run("fsck.erofs", argExtract, "--overwrite", imgPath)
		if err != nil {
			return FsTypeErofs, fmt.Errorf("failed extracting erofs image: %w (output: %s)", err, output)
		}
		return FsTypeErofs, nil

	case FsTypeExt4:
		// 7z extracts ext4 images preserving folder structures
		output, err := tk.Run("7z", "x", imgPath, fmt.Sprintf("-o%s", targetDir), "-y")
		if err != nil {
			if strings.Contains(output, "Dangerous symbolic link path was ignored") {
				_ = os.RemoveAll(filepath.Join(targetDir, "[SYS]"))
				_ = SanitizeSymlinks(targetDir)
				return FsTypeExt4, nil
			}
			return FsTypeExt4, fmt.Errorf("failed extracting ext4 image with 7z: %w (output: %s)", err, output)
		}
		_ = os.RemoveAll(filepath.Join(targetDir, "[SYS]"))
		_ = SanitizeSymlinks(targetDir)
		return FsTypeExt4, nil

	default:
		// Try 7z as fallback
		output, err := tk.Run("7z", "x", imgPath, fmt.Sprintf("-o%s", targetDir), "-y")
		if err == nil {
			_ = os.RemoveAll(filepath.Join(targetDir, "[SYS]"))
			_ = SanitizeSymlinks(targetDir)
			return FsTypeUnknown, nil
		}
		if strings.Contains(output, "Dangerous symbolic link path was ignored") {
			_ = os.RemoveAll(filepath.Join(targetDir, "[SYS]"))
			_ = SanitizeSymlinks(targetDir)
			return FsTypeUnknown, nil
		}
		return FsTypeUnknown, fmt.Errorf("unsupported or unknown filesystem type for %s: %s", imgPath, output)
	}
}

// findFileContexts locates and optionally merges SELinux file_contexts for a given partition.
func findFileContexts(sourceDir, label string) (string, func()) {
	// Look for plat_file_contexts
	platCandidates := []string{
		filepath.Join(sourceDir, "system", "etc", "selinux", "plat_file_contexts"),
		filepath.Join(sourceDir, "etc", "selinux", "plat_file_contexts"),
		filepath.Join(filepath.Dir(sourceDir), "system", "system", "etc", "selinux", "plat_file_contexts"),
		filepath.Join(filepath.Dir(sourceDir), "system", "etc", "selinux", "plat_file_contexts"),
	}

	var platPath string
	for _, p := range platCandidates {
		if stat, err := os.Stat(p); err == nil && !stat.IsDir() {
			platPath = p
			break
		}
	}

	// Look for partition-specific contexts
	var partPath string
	if label != "" && label != "system" {
		partCandidates := []string{
			filepath.Join(sourceDir, "etc", "selinux", fmt.Sprintf("%s_file_contexts", label)),
			filepath.Join(sourceDir, label, "etc", "selinux", fmt.Sprintf("%s_file_contexts", label)),
		}
		for _, p := range partCandidates {
			if stat, err := os.Stat(p); err == nil && !stat.IsDir() {
				partPath = p
				break
			}
		}
	}

	// If both plat and partition contexts exist, merge them
	if platPath != "" && partPath != "" {
		platData, err1 := os.ReadFile(platPath)
		partData, err2 := os.ReadFile(partPath)
		if err1 == nil && err2 == nil {
			tmpFile, err := os.CreateTemp("", "merged_file_contexts_*")
			if err == nil {
				_, _ = tmpFile.Write(platData)
				_, _ = tmpFile.WriteString("\n")
				_, _ = tmpFile.Write(partData)
				_ = tmpFile.Close()
				return tmpFile.Name(), func() { _ = os.Remove(tmpFile.Name()) }
			}
		}
	}

	if platPath != "" && (label == "system" || label == "" || partPath == "") {
		return platPath, nil
	}

	if partPath != "" {
		return partPath, nil
	}

	return "", nil
}

// RepackFS repacks a directory into a filesystem image.
func RepackFS(tk *toolkit.Toolkit, sourceDir, outputImgPath, fsType, label string, minSize int64) error {
	_ = os.Remove(outputImgPath)
	_ = os.RemoveAll(filepath.Join(sourceDir, "[SYS]"))
	_ = SanitizeSymlinks(sourceDir)

	mountPoint := "/"
	if label != "" && label != "system" {
		mountPoint = "/" + label
	}

	contextsPath, cleanup := findFileContexts(sourceDir, label)
	if cleanup != nil {
		defer cleanup()
	}

	switch fsType {
	case FsTypeErofs:
		// mkfs.erofs -z lz4hc,9 -T 1230768000 --all-root --mount-point=<mountPoint> [--file-contexts=<path>] <outputImgPath> <sourceDir>
		args := []string{
			"-z", "lz4hc,9",
			"-T", "1230768000",
			"--all-root",
			fmt.Sprintf("--mount-point=%s", mountPoint),
		}
		if contextsPath != "" {
			args = append(args, fmt.Sprintf("--file-contexts=%s", contextsPath))
		}
		args = append(args, outputImgPath, sourceDir)
		output, err := tk.Run("mkfs.erofs", args...)
		if err != nil {
			return fmt.Errorf("failed repacking erofs: %w (output: %s)", err, output)
		}

	case FsTypeExt4:
		// Calculate required size if minSize <= 0
		var targetSize = minSize
		if targetSize <= 0 {
			dirSize, err := calculateDirSize(sourceDir)
			if err != nil {
				return err
			}
			// Add 25% + 16MB overhead for ext4 metadata/inodes
			targetSize = dirSize + (dirSize / 4) + (16 * 1024 * 1024)
		}
		// Align to 4096 bytes
		targetSize = ((targetSize + 4095) / 4096) * 4096

		if label == "" {
			label = filepath.Base(sourceDir)
		}

		args := []string{
			"-J", // Disable journal on read-only Android partitions (saves 54MB of space)
			"-l", fmt.Sprintf("%d", targetSize),
			"-L", label,
			"-a", mountPoint,
		}
		if contextsPath != "" {
			args = append(args, "-S", contextsPath)
		}
		args = append(args, outputImgPath, sourceDir)
		output, err := tk.Run("make_ext4fs", args...)
		if err != nil {
			return fmt.Errorf("failed repacking ext4: %w (output: %s)", err, output)
		}

	default:
		return fmt.Errorf("unsupported filesystem type for repack: %s", fsType)
	}

	// Verify image was generated and is multiple of 4096 bytes
	stat, err := os.Stat(outputImgPath)
	if err != nil {
		return fmt.Errorf("repacked image not found at %s: %w", outputImgPath, err)
	}

	if stat.Size()%4096 != 0 {
		padBytes := 4096 - (stat.Size() % 4096)
		f, err := os.OpenFile(outputImgPath, os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed opening image for padding: %w", err)
		}
		_, _ = f.Write(bytes.Repeat([]byte{0}, int(padBytes)))
		f.Close()
	}

	return nil
}

func calculateDirSize(dir string) (int64, error) {
	var totalSize int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == "[SYS]" {
			return filepath.SkipDir
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})
	return totalSize, err
}

// SanitizeSymlinks walks dir and normalizes any symlinks containing host paths.
// 7z extracts ext4 symlinks with absolute targets (e.g. /system/bin/init) by prepending
// the host output directory. This function restores them to clean Android target paths.
func SanitizeSymlinks(dir string) error {
	androidRoots := []string{
		"/system/", "/apex/", "/vendor/", "/product/", "/system_ext/",
		"/odm/", "/oem/", "/data/", "/storage/", "/sys/", "/dev/", "/mnt/",
		"/proc/", "/etc/", "/bin/", "/sbin/", "/res/", "/cust/",
	}

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return nil
		}

		target, err := os.Readlink(path)
		if err != nil {
			return nil
		}

		norm := target
		if strings.Contains(norm, "/tmp/") || strings.HasPrefix(norm, "/mnt/") || strings.HasPrefix(norm, "/home/") || strings.HasPrefix(norm, dir) {
			if idx := strings.LastIndex(norm, "/partitions/"); idx != -1 {
				after := norm[idx+len("/partitions/"):]
				if slash := strings.Index(after, "/"); slash != -1 {
					norm = after[slash:]
				}
			}
			if strings.Contains(norm, "/tmp/") || strings.HasPrefix(norm, "/mnt/") || strings.HasPrefix(norm, "/home/") {
				bestIdx := -1
				for _, r := range androidRoots {
					if idx := strings.LastIndex(norm, r); idx > bestIdx {
						bestIdx = idx
					}
				}
				if bestIdx != -1 {
					norm = norm[bestIdx:]
				}
			}
			for strings.HasPrefix(norm, dir) {
				norm = strings.TrimPrefix(norm, dir)
			}
			if !strings.HasPrefix(norm, "/") && !strings.HasPrefix(norm, ".") {
				norm = "/" + norm
			}

			if norm != target {
				_ = os.Remove(path)
				_ = os.Symlink(norm, path)
			}
		}
		return nil
	})
}

