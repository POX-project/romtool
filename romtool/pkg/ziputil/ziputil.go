package ziputil

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"romtool/pkg/toolkit"
)

// ExtractZip extracts a zip file into targetDir safely.
func ExtractZip(zipPath, targetDir string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed opening zip %s: %w", zipPath, err)
	}
	defer reader.Close()

	cleanTarget := filepath.Clean(targetDir)

	for _, file := range reader.File {
		// Prevent ZipSlip
		relPath := filepath.Clean(file.Name)
		if strings.HasPrefix(relPath, "..") || filepath.IsAbs(relPath) {
			continue
		}

		outPath := filepath.Join(cleanTarget, relPath)
		if !strings.HasPrefix(outPath, cleanTarget+string(filepath.Separator)) && outPath != cleanTarget {
			continue
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(outPath, 0755); err != nil {
				return fmt.Errorf("failed creating dir %s: %w", outPath, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
			return fmt.Errorf("failed creating parent dir for %s: %w", outPath, err)
		}

		rc, err := file.Open()
		if err != nil {
			return fmt.Errorf("failed opening zipped file %s: %w", file.Name, err)
		}

		outFile, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			rc.Close()
			return fmt.Errorf("failed creating extracted file %s: %w", outPath, err)
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			return fmt.Errorf("failed writing extracted file %s: %w", outPath, err)
		}

		// Preserve modification time
		_ = os.Chtimes(outPath, file.Modified, file.Modified)
	}

	return nil
}

// CreateZip compresses sourceDir into outZipPath.
func CreateZip(sourceDir, outZipPath string) error {
	if err := os.MkdirAll(filepath.Dir(outZipPath), 0755); err != nil {
		return err
	}

	tempZip := outZipPath + ".tmp"
	_ = os.Remove(tempZip)

	zipFile, err := os.Create(tempZip)
	if err != nil {
		return fmt.Errorf("failed creating zip file %s: %w", tempZip, err)
	}
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	cleanSource := filepath.Clean(sourceDir)

	err = filepath.Walk(cleanSource, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(cleanSource, path)
		if err != nil {
			return err
		}

		if relPath == "." {
			return nil
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return fmt.Errorf("failed creating zip header for %s: %w", path, err)
		}

		// Ensure zip uses forward slashes
		header.Name = filepath.ToSlash(relPath)
		ext := strings.ToLower(filepath.Ext(path))
		if info.IsDir() || ext == ".br" || ext == ".img" || ext == ".apk" || ext == ".dat" {
			header.Method = zip.Store
		} else {
			header.Method = zip.Deflate
		}

		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("failed writing zip header for %s: %w", path, err)
		}

		if !info.IsDir() {
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("failed opening file %s: %w", path, err)
			}
			defer file.Close()

			buf := make([]byte, 256*1024)
			if _, err := io.CopyBuffer(writer, file, buf); err != nil {
				return fmt.Errorf("failed compressing file %s: %w", path, err)
			}
		}

		return nil
	})

	if err != nil {
		_ = os.Remove(tempZip)
		return err
	}

	if err := zipWriter.Close(); err != nil {
		_ = os.Remove(tempZip)
		return err
	}
	if err := zipFile.Close(); err != nil {
		_ = os.Remove(tempZip)
		return err
	}

	return os.Rename(tempZip, outZipPath)
}

// AlignZip aligns the zip using zipalign if available.
func AlignZip(tk *toolkit.Toolkit, inZip, outZip string) error {
	zipalign, err := tk.GetToolPath("zipalign")
	if err != nil {
		// If zipalign not available, simply copy or rename
		if inZip != outZip {
			data, err := os.ReadFile(inZip)
			if err != nil {
				return err
			}
			return os.WriteFile(outZip, data, 0644)
		}
		return nil
	}

	tempOut := outZip + ".aligned"
	_ = os.Remove(tempOut)

	_, err = tk.Run(zipalign, "-f", "4", inZip, tempOut)
	if err != nil {
		// Fallback to plain zip
		if inZip != outZip {
			_ = os.Rename(inZip, outZip)
		}
		return nil
	}

	return os.Rename(tempOut, outZip)
}
