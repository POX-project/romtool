// Resource guards: never freeze the PC. All heavy tools run niced (see
// files.go); these preflights refuse work that would OOM (no-swap boxes
// die fast) or fill the disk mid-image (truncated output flashes badly).
package util

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// MemAvailableBytes reads MemAvailable from /proc/meminfo (0 if unknown).
func MemAvailableBytes() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				return kb << 10
			}
		}
	}
	return 0
}

// DiskFreeBytes returns free bytes on the filesystem holding path.
func DiskFreeBytes(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// SetSelfNice lowers our own CPU priority (best effort, Linux only).
func SetSelfNice(nice int) {
	if runtime.GOOS != "linux" {
		return
	}
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, nice)
}

// SetSelfIdleIO puts our own IO scheduling class to idle (best effort).
func SetSelfIdleIO() {
	if runtime.GOOS != "linux" {
		return
	}
	const sysIoprioSet = 251 // amd64/arm64 Linux
	const ioprioIdle = 3 << 13
	_, _, _ = syscall.Syscall(sysIoprioSet, 1, 0, ioprioIdle)
}
// clamped to [1, ncpu]. The tool default (8) OOMs small boxes on GB payloads.
// payloadWorkers caps payload-dumper concurrency: ~512MB per worker,
// clamped to [1, ncpu]. The tool default (8) OOMs small boxes on GB payloads.
func payloadWorkers() string {
	avail := MemAvailableBytes()
	n := runtime.NumCPU()
	if avail > 0 {
		if w := int(avail / (512 << 20)); w < n {
			n = w
		}
	}
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return strconv.Itoa(n)
}

// DirSize sums file sizes under dir (fast stat walk, no reads).
func DirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// RequireDisk refuses when free space under dir is below needBytes.
// Heavy stages transiently hold ~2x their input (unpack + repack side by
// side), so callers pass the multiplied estimate.
func RequireDisk(dir string, needBytes int64, what string) error {
	free := DiskFreeBytes(dir)
	if free < 0 {
		return nil // unknown filesystem; proceed with caution
	}
	if free < needBytes {
		return fmt.Errorf("%s needs ~%s free under %s, only %s available — refusing (a half-written image flashes badly). Free space or move the project with -dir on a bigger disk",
			what, humanBytes(needBytes), dir, humanBytes(free))
	}
	return nil
}

// WarnMem notes low memory; callers print it, never fail on it
// (available RAM shifts minute to minute; disk is the hard gate).
func WarnMem(minBytes int64) string {
	avail := MemAvailableBytes()
	if avail <= 0 {
		return ""
	}
	if avail < minBytes {
		return fmt.Sprintf("low memory: %s available (no swap detected on OOM risk) — close browsers/builds before flashing-scale stages", humanBytes(avail))
	}
	return ""
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}
