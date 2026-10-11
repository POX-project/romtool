package project

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"romtool/pkg/boot"
	"romtool/pkg/brotli"
	"romtool/pkg/dynamic"
	"romtool/pkg/partition"
	"romtool/pkg/payload"
	"romtool/pkg/sdat"
	"romtool/pkg/toolkit"
	"romtool/pkg/ziputil"
)

const (
	ProjectConfigFile = "rom_project.json"

	RomTypeDynamicBlockOta    = "dynamic_block_ota"
	RomTypeNonDynamicBlockOta = "nondynamic_block_ota"
	RomTypePayload            = "payload"
	RomTypeSuperImg           = "super_img"
	RomTypeRawImages          = "raw_images"
)

type DirSnapshot struct {
	Fingerprint  string    `json:"fingerprint"`
	FileCount    int       `json:"file_count"`
	TotalBytes   int64     `json:"total_bytes"`
	LastScanTime time.Time `json:"last_scan_time"`
}

type PartitionMeta struct {
	Name            string       `json:"name"`
	Format          string       `json:"format"`   // "dat.br", "dat", "img"
	FsType          string       `json:"fs_type"`  // "erofs", "ext4", "unknown"
	OriginalSize    int64        `json:"original_size"`
	CurrentSize     int64        `json:"current_size"`
	Unpacked        bool         `json:"unpacked"`
	UnpackedDir     string       `json:"unpacked_dir"`
	RawImagePath    string       `json:"raw_image_path"`
	Snapshot        *DirSnapshot `json:"snapshot,omitempty"`
	Repacked        bool         `json:"repacked"`
	LastUnpackedAt  time.Time    `json:"last_unpacked_at,omitempty"`
	LastRepackedAt  time.Time    `json:"last_repacked_at,omitempty"`
}

type BootMeta struct {
	Name           string       `json:"name"`
	SourceFile     string       `json:"source_file"`
	Unpacked       bool         `json:"unpacked"`
	WorkDir        string       `json:"work_dir"`
	HasKernel      bool         `json:"has_kernel"`
	HasRamdisk     bool         `json:"has_ramdisk"`
	RamdiskDir     string       `json:"ramdisk_dir"`
	HasDtb         bool         `json:"has_dtb"`
	Snapshot       *DirSnapshot `json:"snapshot,omitempty"`
	Repacked       bool         `json:"repacked"`
	LastUnpackedAt time.Time    `json:"last_unpacked_at,omitempty"`
	LastRepackedAt time.Time    `json:"last_repacked_at,omitempty"`
}

type ProjectManifest struct {
	Version         int                       `json:"version"`
	RomName         string                    `json:"rom_name"`
	SourceRomPath   string                    `json:"source_rom_path"`
	RomType         string                    `json:"rom_type"`
	IsDynamic       bool                      `json:"is_dynamic"`
	DynamicConfig   *dynamic.DynamicConfig    `json:"dynamic_config,omitempty"`
	Partitions      map[string]*PartitionMeta `json:"partitions"`
	BootImages      map[string]*BootMeta      `json:"boot_images"`
	CreatedAt       time.Time                 `json:"created_at"`
	UpdatedAt       time.Time                 `json:"updated_at"`
	LastRomRepackAt time.Time                 `json:"last_rom_repack_at,omitempty"`
	LastRomRepack   string                    `json:"last_rom_repack_file,omitempty"`
}

type Project struct {
	Dir      string           `json:"dir"`
	Manifest *ProjectManifest `json:"manifest"`
	Tk       *toolkit.Toolkit `json:"-"`
}

// ComputeDirSnapshot generates a fingerprint of all files in a directory.
func ComputeDirSnapshot(dir string) (*DirSnapshot, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil
	}

	hasher := sha256.New()
	var count int
	var totalBytes int64

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		fmt.Fprintf(hasher, "%s|%d|%d|%d\n", rel, info.Size(), info.Mode(), info.ModTime().UnixNano())
		if !info.IsDir() {
			count++
			totalBytes += info.Size()
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	return &DirSnapshot{
		Fingerprint:  hex.EncodeToString(hasher.Sum(nil)),
		FileCount:    count,
		TotalBytes:   totalBytes,
		LastScanTime: time.Now(),
	}, nil
}

// Init creates a new project from a ROM zip or folder.
func Init(tk *toolkit.Toolkit, romPath, projectDir string) (*Project, error) {
	if abs, err := filepath.Abs(projectDir); err == nil {
		projectDir = abs
	}

	if err := os.MkdirAll(projectDir, 0755); err != nil {
		return nil, fmt.Errorf("failed creating project dir: %w", err)
	}

	romRoot := filepath.Join(projectDir, "rom_root")
	if err := os.MkdirAll(romRoot, 0755); err != nil {
		return nil, fmt.Errorf("failed creating rom_root: %w", err)
	}

	romName := strings.TrimSuffix(filepath.Base(romPath), filepath.Ext(romPath))

	// Extract ROM if zip
	stat, err := os.Stat(romPath)
	if err != nil {
		return nil, fmt.Errorf("rom path not accessible: %w", err)
	}

	if !stat.IsDir() && strings.HasSuffix(strings.ToLower(romPath), ".zip") {
		fmt.Printf("Extracting ROM zip %s to %s...\n", romPath, romRoot)
		if err := ziputil.ExtractZip(romPath, romRoot); err != nil {
			return nil, fmt.Errorf("failed extracting rom zip: %w", err)
		}
	} else if stat.IsDir() {
		fmt.Printf("Populating ROM root from directory %s...\n", romPath)
		if err := copyOrLinkDir(romPath, romRoot); err != nil {
			return nil, fmt.Errorf("failed populating rom_root: %w", err)
		}
	}

	p := &Project{
		Dir: projectDir,
		Tk:  tk,
		Manifest: &ProjectManifest{
			Version:       1,
			RomName:       romName,
			SourceRomPath: romPath,
			Partitions:    make(map[string]*PartitionMeta),
			BootImages:    make(map[string]*BootMeta),
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		},
	}

	// Detect Dynamic vs Non-Dynamic and Partition layout
	if err := p.analyzeRomRoot(romRoot); err != nil {
		return nil, fmt.Errorf("failed analyzing ROM: %w", err)
	}

	if err := p.Save(); err != nil {
		return nil, err
	}

	return p, nil
}

// Load loads an existing project from projectDir.
func Load(tk *toolkit.Toolkit, projectDir string) (*Project, error) {
	if abs, err := filepath.Abs(projectDir); err == nil {
		projectDir = abs
	}

	cfgPath := filepath.Join(projectDir, ProjectConfigFile)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("project config not found at %s: %w", cfgPath, err)
	}

	var manifest ProjectManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("invalid project config: %w", err)
	}

	p := &Project{
		Dir:      projectDir,
		Manifest: &manifest,
		Tk:       tk,
	}

	return p, nil
}

// Save persists the project manifest to rom_project.json.
func (p *Project) Save() error {
	p.Manifest.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(p.Manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("failed marshaling project config: %w", err)
	}

	cfgPath := filepath.Join(p.Dir, ProjectConfigFile)
	return os.WriteFile(cfgPath, data, 0644)
}

func (p *Project) RomRootDir() string {
	return filepath.Join(p.Dir, "rom_root")
}

func (p *Project) PartitionsDir() string {
	return filepath.Join(p.Dir, "partitions")
}

func (p *Project) RawImagesDir() string {
	return filepath.Join(p.Dir, "raw_images")
}

func (p *Project) BootDir() string {
	return filepath.Join(p.Dir, "boot")
}

func (p *Project) OutputDir() string {
	return filepath.Join(p.Dir, "output")
}

// analyzeRomRoot scans rom_root to detect dynamic or non-dynamic, partition formats, and boot images.
func (p *Project) analyzeRomRoot(romRoot string) error {
	opListPath := filepath.Join(romRoot, "dynamic_partitions_op_list")
	superEmpty := filepath.Join(romRoot, "unsparse_super_empty.img")
	superImg := filepath.Join(romRoot, "super.img")
	payloadBin := filepath.Join(romRoot, "payload.bin")

	// 1. Dynamic detection
	if _, err := os.Stat(opListPath); err == nil {
		p.Manifest.IsDynamic = true
		p.Manifest.RomType = RomTypeDynamicBlockOta
		dynCfg, err := dynamic.ParseOpList(opListPath)
		if err == nil {
			p.Manifest.DynamicConfig = dynCfg
		}
	} else if _, err := os.Stat(superEmpty); err == nil {
		p.Manifest.IsDynamic = true
		p.Manifest.RomType = RomTypeDynamicBlockOta
	} else if _, err := os.Stat(superImg); err == nil {
		p.Manifest.IsDynamic = true
		p.Manifest.RomType = RomTypeSuperImg
	} else if _, err := os.Stat(payloadBin); err == nil {
		p.Manifest.RomType = RomTypePayload
		// Could be dynamic or non-dynamic inside payload
		p.Manifest.IsDynamic = true
	} else {
		p.Manifest.IsDynamic = false
		p.Manifest.RomType = RomTypeNonDynamicBlockOta
	}

	// 2. Discover Partitions
	entries, err := os.ReadDir(romRoot)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}

		if strings.HasSuffix(name, ".new.dat.br") {
			partName := strings.TrimSuffix(name, ".new.dat.br")
			meta := &PartitionMeta{
				Name:         partName,
				Format:       "dat.br",
				UnpackedDir:  filepath.Join(p.PartitionsDir(), partName),
				RawImagePath: filepath.Join(p.RawImagesDir(), partName+".img"),
			}
			if p.Manifest.DynamicConfig != nil {
				meta.OriginalSize = p.Manifest.DynamicConfig.Partitions[partName]
				meta.CurrentSize = meta.OriginalSize
			} else {
				tlPath := filepath.Join(romRoot, partName+".transfer.list")
				if mb, err := sdat.GetMaxBlock(tlPath); err == nil && mb > 0 {
					meta.OriginalSize = mb * sdat.BlockSize
					meta.CurrentSize = meta.OriginalSize
				}
			}
			p.Manifest.Partitions[partName] = meta
		} else if strings.HasSuffix(name, ".new.dat") && !strings.HasSuffix(name, ".new.dat.br") {
			partName := strings.TrimSuffix(name, ".new.dat")
			if _, exists := p.Manifest.Partitions[partName]; !exists {
				meta := &PartitionMeta{
					Name:         partName,
					Format:       "dat",
					UnpackedDir:  filepath.Join(p.PartitionsDir(), partName),
					RawImagePath: filepath.Join(p.RawImagesDir(), partName+".img"),
				}
				if p.Manifest.DynamicConfig != nil {
					meta.OriginalSize = p.Manifest.DynamicConfig.Partitions[partName]
					meta.CurrentSize = meta.OriginalSize
				} else {
					tlPath := filepath.Join(romRoot, partName+".transfer.list")
					if mb, err := sdat.GetMaxBlock(tlPath); err == nil && mb > 0 {
						meta.OriginalSize = mb * sdat.BlockSize
						meta.CurrentSize = meta.OriginalSize
					}
				}
				p.Manifest.Partitions[partName] = meta
			}
		} else if strings.HasSuffix(name, ".img") {
			partName := strings.TrimSuffix(name, ".img")
			// Exclude boot, recovery, dtbo, vbmeta from standard partitions
			if partName == "boot" || partName == "recovery" || partName == "vendor_boot" ||
				partName == "dtbo" || partName == "vbmeta" || partName == "super_dummy" ||
				partName == "unsparse_super_empty" {
				continue
			}
			if _, exists := p.Manifest.Partitions[partName]; !exists {
				meta := &PartitionMeta{
					Name:         partName,
					Format:       "img",
					UnpackedDir:  filepath.Join(p.PartitionsDir(), partName),
					RawImagePath: filepath.Join(romRoot, name),
				}
				if st, err := os.Stat(filepath.Join(romRoot, name)); err == nil {
					meta.OriginalSize = st.Size()
					meta.CurrentSize = st.Size()
				}
				p.Manifest.Partitions[partName] = meta
			}
		}
	}

	// 3. Discover Boot Images
	bootCandidates := []string{"boot.img", "recovery.img", "vendor_boot.img"}
	for _, b := range bootCandidates {
		bPath := filepath.Join(romRoot, b)
		if _, err := os.Stat(bPath); err == nil {
			name := strings.TrimSuffix(b, ".img")
			p.Manifest.BootImages[name] = &BootMeta{
				Name:       name,
				SourceFile: b,
				WorkDir:    filepath.Join(p.BootDir(), name),
				RamdiskDir: filepath.Join(p.BootDir(), name, "ramdisk"),
			}
		}
	}

	return nil
}

// IsPartitionModified checks if files in partitions/<name> changed compared to stored snapshot.
func (p *Project) IsPartitionModified(name string) (bool, error) {
	meta, ok := p.Manifest.Partitions[name]
	if !ok || !meta.Unpacked {
		return false, nil
	}

	partDir := filepath.Join(p.PartitionsDir(), name)
	currentSnap, err := ComputeDirSnapshot(partDir)
	if err != nil {
		return false, err
	}
	if currentSnap == nil {
		return false, nil
	}

	if meta.Snapshot == nil {
		return true, nil
	}

	return currentSnap.Fingerprint != meta.Snapshot.Fingerprint, nil
}

// IsBootModified checks if boot image ramdisk or files changed.
func (p *Project) IsBootModified(name string) (bool, error) {
	meta, ok := p.Manifest.BootImages[name]
	if !ok || !meta.Unpacked {
		return false, nil
	}

	ramdiskDir := filepath.Join(p.BootDir(), name, "ramdisk")
	currentSnap, err := ComputeDirSnapshot(ramdiskDir)
	if err != nil {
		return false, err
	}
	if currentSnap == nil {
		return false, nil
	}

	if meta.Snapshot == nil {
		return true, nil
	}

	return currentSnap.Fingerprint != meta.Snapshot.Fingerprint, nil
}

// UnpackPartition unpacks a specific partition all the way to its editable filesystem directory.
func (p *Project) UnpackPartition(name string) error {
	meta, ok := p.Manifest.Partitions[name]
	if !ok {
		return fmt.Errorf("partition %q not recognized", name)
	}

	romRoot := p.RomRootDir()
	rawImgDir := p.RawImagesDir()
	_ = os.MkdirAll(rawImgDir, 0755)

	rawImgPath := filepath.Join(rawImgDir, name+".img")

	switch meta.Format {
	case "dat.br":
		newDatBrPath := filepath.Join(romRoot, name+".new.dat.br")
		tlPath := filepath.Join(romRoot, name+".transfer.list")
		tempNewDat := filepath.Join(rawImgDir, name+".new.dat")
		defer os.Remove(tempNewDat)

		fmt.Printf("[%s] Decompressing %s...\n", name, filepath.Base(newDatBrPath))
		if err := brotli.DecompressFile(newDatBrPath, tempNewDat); err != nil {
			return fmt.Errorf("failed decompressing %s: %w", newDatBrPath, err)
		}

		fmt.Printf("[%s] Converting transfer list + new.dat -> raw image %s...\n", name, filepath.Base(rawImgPath))
		if err := sdat.Sdat2Img(tlPath, tempNewDat, rawImgPath); err != nil {
			return fmt.Errorf("failed sdat2img for %s: %w", name, err)
		}

	case "dat":
		newDatPath := filepath.Join(romRoot, name+".new.dat")
		tlPath := filepath.Join(romRoot, name+".transfer.list")
		fmt.Printf("[%s] Converting transfer list + new.dat -> raw image %s...\n", name, filepath.Base(rawImgPath))
		if err := sdat.Sdat2Img(tlPath, newDatPath, rawImgPath); err != nil {
			return fmt.Errorf("failed sdat2img for %s: %w", name, err)
		}

	case "img":
		rawImgPath = filepath.Join(romRoot, name+".img")

	case "payload":
		payloadBin := filepath.Join(romRoot, "payload.bin")
		fmt.Printf("[%s] Extracting from payload.bin...\n", name)
		if err := payload.ExtractPayload(p.Tk, payloadBin, rawImgDir, []string{name}); err != nil {
			return fmt.Errorf("failed extracting %s from payload.bin: %w", name, err)
		}
	}

	// Detect and extract filesystem
	fsType, err := partition.DetectFsType(rawImgPath)
	if err != nil {
		return err
	}
	meta.FsType = fsType

	targetDir := filepath.Join(p.PartitionsDir(), name)
	fmt.Printf("[%s] Extracting %s filesystem to %s...\n", name, strings.ToUpper(fsType), targetDir)
	detectedFs, err := partition.ExtractFS(p.Tk, rawImgPath, targetDir)
	if err != nil {
		return fmt.Errorf("failed extracting filesystem: %w", err)
	}
	meta.FsType = detectedFs

	// Record size and directory snapshot
	if stat, err := os.Stat(rawImgPath); err == nil {
		meta.CurrentSize = stat.Size()
		if meta.OriginalSize == 0 {
			meta.OriginalSize = stat.Size()
		}
	}

	snap, _ := ComputeDirSnapshot(targetDir)
	meta.Snapshot = snap
	meta.Unpacked = true
	meta.UnpackedDir = targetDir
	meta.RawImagePath = rawImgPath
	meta.Repacked = false
	meta.LastUnpackedAt = time.Now()

	return p.Save()
}

// RepackPartition repacks an editable partition directory back into .img and .new.dat.br,
// and automatically recalculates dynamic partition op list sizes if dynamic!
func (p *Project) RepackPartition(name string) error {
	meta, ok := p.Manifest.Partitions[name]
	if !ok {
		return fmt.Errorf("partition %q not recognized", name)
	}
	if !meta.Unpacked {
		return fmt.Errorf("partition %q has not been unpacked yet", name)
	}

	rawImgDir := p.RawImagesDir()
	rawImgPath := filepath.Join(rawImgDir, name+".img")
	sourceDir := meta.UnpackedDir
	if !filepath.IsAbs(sourceDir) {
		candidates := []string{
			filepath.Join(p.Dir, "partitions", name),
			filepath.Join(p.Dir, filepath.Base(sourceDir)),
			filepath.Join(filepath.Dir(p.Dir), sourceDir),
			sourceDir,
		}
		for _, c := range candidates {
			if stat, err := os.Stat(c); err == nil && stat.IsDir() {
				sourceDir = c
				break
			}
		}
	}

	fmt.Printf("[%s] Repacking directory %s to %s (%s)...\n", name, sourceDir, filepath.Base(rawImgPath), meta.FsType)
	if err := partition.RepackFS(p.Tk, sourceDir, rawImgPath, meta.FsType, name, meta.OriginalSize); err != nil {
		return fmt.Errorf("failed repacking filesystem for %s: %w", name, err)
	}

	stat, err := os.Stat(rawImgPath)
	if err != nil {
		return fmt.Errorf("repacked image missing: %w", err)
	}
	newSize := stat.Size()
	meta.CurrentSize = newSize

	romRoot := p.RomRootDir()

	if meta.Format == "dat.br" || meta.Format == "dat" {
		fmt.Printf("[%s] Converting raw image -> transfer list & new.dat...\n", name)
		res, err := sdat.Img2Sdat(rawImgPath, romRoot, name, 1024)
		if err != nil {
			return fmt.Errorf("img2sdat failed for %s: %w", name, err)
		}
		newSize = res.ImageSize
		meta.CurrentSize = newSize

		if meta.Format == "dat.br" {
			newDatPath := filepath.Join(romRoot, name+".new.dat")
			newDatBrPath := filepath.Join(romRoot, name+".new.dat.br")
			fmt.Printf("[%s] Compressing to %s...\n", name, filepath.Base(newDatBrPath))
			if err := brotli.CompressFile(newDatPath, newDatBrPath, 1); err != nil {
				return fmt.Errorf("failed brotli compression for %s: %w", name, err)
			}
			_ = os.Remove(newDatPath) // Cleanup uncompressed .new.dat
		}
	}

	// MAGIC STEP: If Dynamic ROM, update dynamic_partitions_op_list!
	if p.Manifest.IsDynamic {
		opListPath := filepath.Join(romRoot, "dynamic_partitions_op_list")
		if _, err := os.Stat(opListPath); err == nil {
			fmt.Printf("[%s] Dynamic ROM detected: Updating dynamic_partitions_op_list with new size %d bytes (was %d)...\n",
				name, newSize, meta.OriginalSize)
			dynCfg, err := dynamic.ParseOpList(opListPath)
			if err != nil {
				return fmt.Errorf("failed parsing dynamic_partitions_op_list: %w", err)
			}
			if err := dynCfg.UpdatePartitionSize(name, newSize); err != nil {
				return fmt.Errorf("dynamic partition validation failed: %w", err)
			}
			if err := dynCfg.Save(); err != nil {
				return fmt.Errorf("failed saving dynamic_partitions_op_list: %w", err)
			}
			p.Manifest.DynamicConfig = dynCfg
		}
	}

	// If vbmeta.img is present, disable AVB verification to allow booting modified partitions
	vbmetaPath := filepath.Join(romRoot, "vbmeta.img")
	if _, err := os.Stat(vbmetaPath); err == nil {
		_ = p.PatchVBMetaDisableVerification(vbmetaPath)
	}

	// Update snapshot and state
	snap, _ := ComputeDirSnapshot(sourceDir)
	meta.Snapshot = snap
	meta.Repacked = true
	meta.LastRepackedAt = time.Now()

	return p.Save()
}

// UnpackBootImage unpacks a boot/recovery image to its work directory and extracts ramdisk.
func (p *Project) UnpackBootImage(name string) error {
	meta, ok := p.Manifest.BootImages[name]
	if !ok {
		return fmt.Errorf("boot image %q not recognized", name)
	}

	bootPath := filepath.Join(p.RomRootDir(), meta.SourceFile)
	workDir := meta.WorkDir

	fmt.Printf("[%s] Unpacking %s using magiskboot...\n", name, meta.SourceFile)
	bMeta, err := boot.UnpackBoot(p.Tk, bootPath, workDir)
	if err != nil {
		return fmt.Errorf("failed unpacking boot image: %w", err)
	}

	meta.Unpacked = true
	meta.HasKernel = bMeta.HasKernel
	meta.HasRamdisk = bMeta.HasRamdisk
	meta.HasDtb = bMeta.HasDtb
	meta.RamdiskDir = bMeta.RamdiskDir
	meta.Repacked = false
	meta.LastUnpackedAt = time.Now()

	if meta.HasRamdisk {
		snap, _ := ComputeDirSnapshot(meta.RamdiskDir)
		meta.Snapshot = snap
	}

	return p.Save()
}

// RepackBootImage repacks the ramdisk and boot image using magiskboot.
func (p *Project) RepackBootImage(name string) error {
	meta, ok := p.Manifest.BootImages[name]
	if !ok {
		return fmt.Errorf("boot image %q not recognized", name)
	}
	if !meta.Unpacked {
		return fmt.Errorf("boot image %q has not been unpacked yet", name)
	}

	origBootPath := filepath.Join(p.RomRootDir(), meta.SourceFile)
	targetBootPath := filepath.Join(p.RomRootDir(), meta.SourceFile)

	fmt.Printf("[%s] Repacking %s with magiskboot...\n", name, meta.SourceFile)
	if err := boot.RepackBoot(p.Tk, origBootPath, meta.WorkDir, targetBootPath); err != nil {
		return fmt.Errorf("failed repacking boot image %s: %w", name, err)
	}

	if meta.HasRamdisk {
		snap, _ := ComputeDirSnapshot(meta.RamdiskDir)
		meta.Snapshot = snap
	}
	meta.Repacked = true
	meta.LastRepackedAt = time.Now()

	return p.Save()
}

// UnpackAll unpacks all partitions and boot images.
func (p *Project) UnpackAll() error {
	for name := range p.Manifest.Partitions {
		if err := p.UnpackPartition(name); err != nil {
			return err
		}
	}
	for name := range p.Manifest.BootImages {
		if err := p.UnpackBootImage(name); err != nil {
			return err
		}
	}
	return nil
}

// RepackModified detects modified partitions/boot images and repacks them automatically.
func (p *Project) RepackModified(forceAll bool) ([]string, error) {
	var repacked []string

	for name, meta := range p.Manifest.Partitions {
		if !meta.Unpacked {
			continue
		}
		modified, err := p.IsPartitionModified(name)
		if err != nil {
			return nil, err
		}
		if modified || forceAll || !meta.Repacked {
			if err := p.RepackPartition(name); err != nil {
				return nil, err
			}
			repacked = append(repacked, name)
		}
	}

	for name, meta := range p.Manifest.BootImages {
		if !meta.Unpacked {
			continue
		}
		modified, err := p.IsBootModified(name)
		if err != nil {
			return nil, err
		}
		if modified || forceAll || !meta.Repacked {
			if err := p.RepackBootImage(name); err != nil {
				return nil, err
			}
			repacked = append(repacked, name)
		}
	}

	return repacked, nil
}

// RepackRomZip packages rom_root into a flashable zip and applies zipalign.
func (p *Project) RepackRomZip(outputZipPath string) error {
	// First ensure any pending modifications are repacked
	repacked, err := p.RepackModified(false)
	if err != nil {
		return fmt.Errorf("failed repacking modified components before zip packaging: %w", err)
	}
	if len(repacked) > 0 {
		fmt.Printf("Automatically repacked modified components: %s\n", strings.Join(repacked, ", "))
	}

	// Ensure vbmeta disables verification if present
	vbmetaPath := filepath.Join(p.RomRootDir(), "vbmeta.img")
	if _, err := os.Stat(vbmetaPath); err == nil {
		_ = p.PatchVBMetaDisableVerification(vbmetaPath)
	}

	if outputZipPath == "" {
		_ = os.MkdirAll(p.OutputDir(), 0755)
		timestamp := time.Now().Format("20060102_150405")
		outputZipPath = filepath.Join(p.OutputDir(), fmt.Sprintf("%s-repacked-%s.zip", p.Manifest.RomName, timestamp))
	}

	fmt.Printf("Building flashable ROM zip -> %s...\n", outputZipPath)
	if err := ziputil.CreateZip(p.RomRootDir(), outputZipPath); err != nil {
		return fmt.Errorf("failed creating zip: %w", err)
	}

	// Run zipalign if available
	fmt.Printf("Aligning zip packages...\n")
	_ = ziputil.AlignZip(p.Tk, outputZipPath, outputZipPath)

	p.Manifest.LastRomRepackAt = time.Now()
	p.Manifest.LastRomRepack = outputZipPath
	_ = p.Save()

	stat, _ := os.Stat(outputZipPath)
	var sizeMB float64
	if stat != nil {
		sizeMB = float64(stat.Size()) / (1024 * 1024)
	}

	fmt.Printf("\nSUCCESS: Repacked ROM zip created successfully!\n")
	fmt.Printf("Output File: %s\n", outputZipPath)
	fmt.Printf("Size:        %.2f MB\n", sizeMB)
	fmt.Printf("Dynamic:     %v\n", p.Manifest.IsDynamic)
	fmt.Printf("ROM Type:    %s\n", p.Manifest.RomType)

	return nil
}

// PatchVBMetaDisableVerification patches a vbmeta.img to disable AVB dm-verity and verification.
func (p *Project) PatchVBMetaDisableVerification(vbmetaPath string) error {
	stat, err := os.Stat(vbmetaPath)
	if err != nil {
		return err
	}

	if p.Tk.Has("avbtool") {
		padding := fmt.Sprintf("%d", stat.Size())
		if stat.Size() < 4096 {
			padding = "4096"
		}
		tempOut := vbmetaPath + ".disabled.tmp"
		output, err := p.Tk.Run("avbtool", "make_vbmeta_image",
			"--flags", "3",
			"--include_descriptors_from_image", vbmetaPath,
			"--padding_size", padding,
			"--output", tempOut,
		)
		if err == nil {
			_ = os.Rename(tempOut, vbmetaPath)
			fmt.Printf("Successfully patched vbmeta.img with avbtool (flags: 3 - verification & hashtree disabled).\n")
			return nil
		}
		fmt.Printf("avbtool patch warning (%v: %s), falling back to binary patch...\n", err, output)
	}

	// Binary patch fallback: AVB magic "AVB0", flags is uint32 big-endian at offset 120 (0x78)
	data, err := os.ReadFile(vbmetaPath)
	if err != nil {
		return err
	}
	if len(data) >= 124 && string(data[0:4]) == "AVB0" {
		currentFlags := binary.BigEndian.Uint32(data[120:124])
		newFlags := currentFlags | 3
		binary.BigEndian.PutUint32(data[120:124], newFlags)
		if err := os.WriteFile(vbmetaPath, data, 0644); err != nil {
			return err
		}
		fmt.Printf("Binary patched vbmeta.img flags from %d to %d (verification disabled)\n", currentFlags, newFlags)
		return nil
	}

	return fmt.Errorf("invalid vbmeta.img format")
}

func copyOrLinkDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		_ = os.MkdirAll(filepath.Dir(target), 0755)
		// Try hard link first (fast, 0 disk space)
		if err := os.Link(path, target); err == nil {
			return nil
		}
		// Fallback to copy
		inF, err := os.Open(path)
		if err != nil {
			return err
		}
		defer inF.Close()
		outF, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer outF.Close()
		_, err = io.Copy(outF, inF)
		return err
	})
}
