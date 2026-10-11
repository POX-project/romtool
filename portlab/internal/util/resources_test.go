package util

import (
	"strconv"
	"testing"
)

func TestPayloadWorkersSane(t *testing.T) {
	w, err := strconv.Atoi(payloadWorkers())
	if err != nil || w < 1 || w > 8 {
		t.Fatalf("workers out of range: %q %v", payloadWorkers(), err)
	}
}

func TestRequireDisk(t *testing.T) {
	dir := t.TempDir()
	if err := RequireDisk(dir, 1<<30, "test"); err != nil {
		t.Skipf("tiny tmpfs? %v", err)
	}
	if err := RequireDisk(dir, 1<<60, "test"); err == nil {
		t.Fatal("impossible size must be refused")
	}
	if DiskFreeBytes(dir) <= 0 {
		t.Fatal("disk free must be positive")
	}
}

func TestDirSize(t *testing.T) {
	if DirSize(t.TempDir()) != 0 {
		t.Fatal("empty dir size must be 0")
	}
}
