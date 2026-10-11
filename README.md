# POX PROJECT - ROM Tool & Repacker (`romtool`)

[![Build and Release](https://github.com/POX-project/romtool/actions/workflows/build.yml/badge.svg)](https://github.com/POX-project/romtool/actions/workflows/build.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Latest Release](https://img.shields.io/github/v/release/POX-project/romtool?color=brightgreen)](https://github.com/POX-project/romtool/releases)

**Maintainer:** **`txorav`** • **Language:** Go 1.22+ • **Platform:** Linux x86_64


A high-performance, modular Android ROM unpacker and repacker written in **Go**, designed for modern Android custom ROMs (Android 10 - 16+), supporting both **Dynamic Partitions** (`super.img`, `dynamic_partitions_op_list`, `payload.bin`) and **Non-Dynamic Partitions**.

It features an intelligent **JSON State & Change Tracker** (`rom_project.json`), a **self-contained portable dev toolkit** (`toolkit/bin/`), and an **interactive terminal dashboard**.

---

## Key Features

1. **Intelligent JSON Tracking (`rom_project.json`)**:
   - Maintains complete metadata across unpack, user modifications, and repack phases.
   - Tracks file count, directory sizes, and recursive hash fingerprints.
   - **Automatic Change Detection**: Automatically detects whenever you modify, add, or delete files inside `partitions/<name>/` or `boot/<name>/ramdisk/`.
   - **Smart Repacking**: Only repacks components that you actually modified (or force all if desired).

2. **Automatic Dynamic vs. Non-Dynamic Partition Handling**:
   - **Dynamic ROMs**:
     - Automatically parses `dynamic_partitions_op_list`, `unsparse_super_empty.img`, and group limits (`add_group`, `resize`, `add`).
     - When you modify files in an EROFS/EXT4 partition, the new image size changes. `romtool` **automatically recalculates the exact byte size** and **updates `resize <partition> <new_size_in_bytes>`** in `dynamic_partitions_op_list`.
     - Validates total partition sizes against the group maximum capacity to prevent flashing errors.
   - **Non-Dynamic ROMs**:
     - Automatically detects direct partition mappings in `updater-script`.
     - Repacks `.new.dat` / `.new.dat.br` without dynamic partition operations.

3. **Multi-Format Filesystem Support**:
   - **EROFS**: Modern read-only Android filesystem (used in Android 13 - 16+ ROMs, like Lunaris Begonia). Extracted via `fsck.erofs` and repacked via `mkfs.erofs -z lz4hc,9`.
   - **EXT4**: Extracted via `7z` and repacked via `make_ext4fs`.
   - **Android Sparse Data**: Pure Go streaming implementation of `sdat2img` and `img2sdat` with 4MB chunking.
   - **Brotli Compression**: Native multi-threaded streaming Brotli decompressor and compressor in Go.

4. **Boot / Recovery Image Extraction & Repack**:
   - Integrated with Magisk's `magiskboot`.
   - Automatically unpacks `kernel`, `dtb`, `header`, and extracts `ramdisk.cpio` into an editable directory: `boot/<name>/ramdisk/`.
   - Repacks the edited ramdisk directory back into `ramdisk.cpio` and generates the updated `boot.img`.

5. **Self-Contained & Portable Toolkit**:
   - Bundles all required prebuilt static binaries in `./toolkit/bin/`:
     - `magiskboot`, `mkfs.erofs`, `fsck.erofs`, `make_ext4fs`, `lpmake`, `lpunpack`, `simg2img`, `img2simg`, `payload-dumper-go`, `zipalign`, `cpio`.
   - Statically compiled Go binary (`CGO_ENABLED=0`) with zero external dependencies.

---

## Directory Layout

```
.
├── bin/
│   └── romtool                       # Statically linked standalone binary
├── toolkit/
│   └── bin/                          # Portable Android tool binaries
├── romtool/                          # Full Go source code
│   ├── cmd/romtool/main.go
│   └── pkg/
│       ├── boot/                     # Magiskboot & ramdisk handler
│       ├── brotli/                   # Native Go Brotli compressor/decompressor
│       ├── dynamic/                  # Dynamic partitions op list parser & updater
│       ├── menu/                     # Interactive TUI dashboard
│       ├── partition/                # EROFS / EXT4 extraction & repacking
│       ├── payload/                  # Android payload.bin extractor
│       ├── project/                  # JSON manifest & change-detection engine
│       ├── sdat/                     # Pure Go sdat2img & img2sdat
│       ├── toolkit/                  # Tool resolution & runner
│       └── ziputil/                  # Safe zip extraction, creation & zipalign
├── original images/
│   └── Lunaris-AOSP-begonia-UNOFFICIAL-3.12-GMS-2026091209.zip
├── build.sh                          # Rebuild script
└── run.sh                            # Convenience runner
```

---

## Project Structure (When Unpacked)

```
my_rom_project/
├── rom_project.json                  # JSON state tracker (dynamic info, sizes, hashes)
├── rom_root/                         # Extracted ROM zip root
│   ├── META-INF/
│   ├── boot.img
│   ├── dynamic_partitions_op_list   # Automatically updated on repack
│   ├── system.new.dat.br
│   └── ...
├── raw_images/                       # Converted .img files (system.img, odm.img, etc.)
├── partitions/                       # EDITABLE FILESYSTEM TREES!
│   ├── system/
│   ├── vendor/
│   ├── product/
│   └── odm/
├── boot/                             # EDITABLE BOOT COMPONENTS
│   └── boot/
│       ├── kernel
│       ├── dtb
│       └── ramdisk/                  # EDITABLE RAMDISK (init.rc, fstab, etc.)
└── output/                           # Flashable repacked ROM zips
    └── Lunaris-begonia-repacked.zip
```

---

## Quick Start & Usage

### 1. Interactive TUI Menu Mode

Simply run `./run.sh` or `./bin/romtool`:
```bash
./run.sh menu -project ./my_project
```
This displays the interactive project dashboard with ANSI color indicators:
```
==================================================================
              ANDROID ROM TOOLKIT & REPACKER                      
==================================================================
 Project:  /path/to/my_project
 ROM Name: Lunaris-begonia [DYNAMIC ROM]
 Format:   dynamic_block_ota | JSON Tracker: rom_project.json
 Dynamic Group: xiaomi_dynamic_partitions (4369.3 MB / 5116.0 MB max)
------------------------------------------------------------------
PARTITION      FORMAT   FS       SIZE       UNPACKED   STATUS      
------------------------------------------------------------------
odm            dat.br   erofs    30.3       Yes        MODIFIED
product        dat.br   -        1744.5     No         Clean
system         dat.br   -        1082.4     No         Clean
system_ext     dat.br   -        1121.0     No         Clean
vendor         dat.br   -        420.9      No         Clean
------------------------------------------------------------------
BOOT IMAGE     SOURCE   RAMDISK  KERNEL     UNPACKED   STATUS      
------------------------------------------------------------------
boot           boot.img Yes      Yes        Yes        Clean
==================================================================

Actions:
 [1] Detailed Project & File Overview
 [2] Unpack All (Partitions + Boot Images)
 [3] Unpack Specific Partition (e.g. system, vendor, product, odm)
 [4] Unpack Boot Image (kernel & editable ramdisk)
 [5] Check for Modifications in partitions/ and boot/
 [6] Repack Modified Partitions & Boot (Dynamic recalculation)
 [7] Repack Specific Partition
 [8] Repack Boot Image
 [9] Build Flashable ROM Zip (Zipalign included)
 [D] Doctor / Check Toolkit & Tools
 [0] Exit
```

---

### 2. Command Line Usage (CLI & Scripting)

#### Step 1: Initialize Project from ROM Zip
```bash
./run.sh init -rom "/path/to/Lunaris-AOSP-begonia.zip" -dir ./projects/my_begonia_rom
```

#### Step 2: Unpack Partition(s)
Unpack a specific partition (e.g. `odm` or `system`):
```bash
./run.sh unpack-partition -project ./projects/my_begonia_rom -name odm
```
Or unpack everything:
```bash
./run.sh unpack-all -project ./projects/my_begonia_rom
```

#### Step 3: Unpack Boot Image
```bash
./run.sh unpack-boot -project ./projects/my_begonia_rom
```
Now you can edit files inside:
- `projects/my_begonia_rom/partitions/odm/`
- `projects/my_begonia_rom/boot/boot/ramdisk/`

#### Step 4: Check What Has Changed
```bash
./run.sh check -project ./projects/my_begonia_rom
```

#### Step 5: Repack Modified Files
Automatically repacks only modified partitions and boot images, and updates `dynamic_partitions_op_list`:
```bash
./run.sh repack -project ./projects/my_begonia_rom
```

#### Step 6: Build Flashable ROM Zip
Packages the complete ROM and applies 4-byte zipalign:
```bash
./run.sh repack-rom -project ./projects/my_begonia_rom -output ./output/Custom-Lunaris-Begonia.zip
```

---

### 3. Standalone Utility Subcommands

- Check toolkit health:
  ```bash
  ./run.sh doctor
  ```
- Convert transfer list + new.dat to raw image:
  ```bash
  ./run.sh sdat2img system.transfer.list system.new.dat system.img
  ```
- Convert raw image to transfer list + new.dat:
  ```bash
  ./run.sh img2sdat -img system.img -name system -out ./output
  ```

---

## Rebuilding

To rebuild the Go binary at any time:
```bash
./build.sh
```

---

## 🇵🇸 Solidarity
**FREE PALESTINE** — Support justice, end oppression, and spread awareness on all matters of shared humanity.
