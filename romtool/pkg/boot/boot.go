package boot

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"romtool/pkg/toolkit"
)

type BootMetadata struct {
	Name        string `json:"name"`
	WorkDir     string `json:"work_dir"`
	HasKernel   bool   `json:"has_kernel"`
	HasRamdisk  bool   `json:"has_ramdisk"`
	RamdiskDir  string `json:"ramdisk_dir"`
	HasDtb      bool   `json:"has_dtb"`
	HasHeader   bool   `json:"has_header"`
	Repacked    bool   `json:"repacked"`
}

// UnpackBoot unpacks a boot/recovery/vendor_boot image using magiskboot.
func UnpackBoot(tk *toolkit.Toolkit, bootImgPath, workDir string) (*BootMetadata, error) {
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed creating boot work dir %s: %w", workDir, err)
	}

	// Run magiskboot unpack <bootImgPath> in workDir
	output, err := tk.RunInDir(workDir, "magiskboot", "unpack", "-h", bootImgPath)
	if err != nil {
		return nil, fmt.Errorf("magiskboot unpack failed: %w (output: %s)", err, output)
	}

	meta := &BootMetadata{
		Name:    filepath.Base(bootImgPath),
		WorkDir: workDir,
	}

	if _, err := os.Stat(filepath.Join(workDir, "kernel")); err == nil {
		meta.HasKernel = true
	}
	if _, err := os.Stat(filepath.Join(workDir, "dtb")); err == nil {
		meta.HasDtb = true
	}
	if _, err := os.Stat(filepath.Join(workDir, "header")); err == nil {
		meta.HasHeader = true
	}

	ramdiskCpio := filepath.Join(workDir, "ramdisk.cpio")
	if _, err := os.Stat(ramdiskCpio); err == nil {
		meta.HasRamdisk = true
		ramdiskDir := filepath.Join(workDir, "ramdisk")
		meta.RamdiskDir = ramdiskDir
		_ = os.MkdirAll(ramdiskDir, 0755)

		// Extract ramdisk.cpio to ramdiskDir
		// Use magiskboot cpio ramdisk.cpio extract
		_, err := tk.RunInDir(ramdiskDir, "magiskboot", "cpio", ramdiskCpio, "extract")
		if err != nil {
			// Fallback to cpio tool
			cpioPath, cErr := tk.GetToolPath("cpio")
			if cErr == nil {
				cmd := exec.Command(cpioPath, "-idmv")
				cmd.Dir = ramdiskDir
				cpioData, rErr := os.ReadFile(ramdiskCpio)
				if rErr == nil {
					cmd.Stdin = bytes.NewReader(cpioData)
					_ = cmd.Run()
				}
			}
		}
	}

	return meta, nil
}

// RepackBoot repacks boot components in workDir back into outBootImgPath.
func RepackBoot(tk *toolkit.Toolkit, origBootImgPath, workDir, outBootImgPath string) error {
	ramdiskDir := filepath.Join(workDir, "ramdisk")
	ramdiskCpio := filepath.Join(workDir, "ramdisk.cpio")

	// If ramdisk dir exists and has entries, pack it to ramdisk.cpio
	if entries, err := os.ReadDir(ramdiskDir); err == nil && len(entries) > 0 {
		cpioBin, err := tk.GetToolPath("cpio")
		if err == nil {
			// find . -mindepth 1 | cpio -o -H newc > ../ramdisk.cpio
			cmdFind := exec.Command("find", ".", "-mindepth", "1")
			cmdFind.Dir = ramdiskDir

			cmdCpio := exec.Command(cpioBin, "-o", "-H", "newc")
			cmdCpio.Dir = ramdiskDir

			pipe, err := cmdFind.StdoutPipe()
			if err == nil {
				cmdCpio.Stdin = pipe
				var cpioOut bytes.Buffer
				cmdCpio.Stdout = &cpioOut

				if err := cmdFind.Start(); err == nil {
					if err := cmdCpio.Start(); err == nil {
						_ = cmdFind.Wait()
						_ = cmdCpio.Wait()
						if cpioOut.Len() > 0 {
							_ = os.WriteFile(ramdiskCpio, cpioOut.Bytes(), 0644)
						}
					}
				}
			}
		}
	}

	// Repack with magiskboot
	tempOut := filepath.Join(workDir, "new-boot.img")
	_ = os.Remove(tempOut)

	output, err := tk.RunInDir(workDir, "magiskboot", "repack", origBootImgPath, tempOut)
	if err != nil {
		return fmt.Errorf("magiskboot repack failed: %w (output: %s)", err, output)
	}

	// Check if new-boot.img was created
	if _, err := os.Stat(tempOut); err != nil {
		return fmt.Errorf("repacked boot image not found at %s: %w", tempOut, err)
	}

	// Copy to outBootImgPath
	if err := os.MkdirAll(filepath.Dir(outBootImgPath), 0755); err != nil {
		return err
	}
	data, err := os.ReadFile(tempOut)
	if err != nil {
		return err
	}

	// Samsung S-Boot TIMA trusted boot requires SEANDROIDENFORCE magic footer.
	origBootData, _ := os.ReadFile(origBootImgPath)
	if bytes.Contains(origBootData, []byte("SEANDROIDENFORCE")) || bytes.Contains(origBootData, []byte("universal7904")) || bytes.Contains(origBootData, []byte("samsung")) || bytes.Contains(origBootData, []byte("exynos")) {
		if !bytes.Contains(data, []byte("SEANDROIDENFORCE")) {
			data = append(data, []byte("SEANDROIDENFORCE")...)
		}
	}

	return os.WriteFile(outBootImgPath, data, 0644)
}
