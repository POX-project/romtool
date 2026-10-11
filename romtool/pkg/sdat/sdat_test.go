package sdat_test

import (
	"os"
	"path/filepath"
	"testing"

	"romtool/pkg/brotli"
	"romtool/pkg/sdat"
)

func TestSdatE2E(t *testing.T) {
	origDir := "/mnt/9f4d8733-a954-435d-b92f-f33c068c3b1e/repack bego/projects/lunaris 3.12/test"
	brPath := filepath.Join(origDir, "odm.new.dat.br")
	tlPath := filepath.Join(origDir, "odm.transfer.list")

	if _, err := os.Stat(brPath); os.IsNotExist(err) {
		t.Skip("Original odm files not found, skipping e2e test")
	}

	tmpDir := t.TempDir()
	newDatPath := filepath.Join(tmpDir, "odm.new.dat")
	outImgPath := filepath.Join(tmpDir, "odm.img")

	// 1. Decompress brotli
	err := brotli.DecompressFile(brPath, newDatPath)
	if err != nil {
		t.Fatalf("DecompressFile failed: %v", err)
	}

	// 2. Sdat2Img
	err = sdat.Sdat2Img(tlPath, newDatPath, outImgPath)
	if err != nil {
		t.Fatalf("Sdat2Img failed: %v", err)
	}

	stat, err := os.Stat(outImgPath)
	if err != nil {
		t.Fatalf("Stat outImg failed: %v", err)
	}
	if stat.Size() != 31789056 {
		t.Fatalf("Expected image size 31789056, got %d", stat.Size())
	}

	// 3. Img2Sdat
	repackDir := filepath.Join(tmpDir, "repack")
	res, err := sdat.Img2Sdat(outImgPath, repackDir, "odm", 1024)
	if err != nil {
		t.Fatalf("Img2Sdat failed: %v", err)
	}
	if res.ImageSize != 31789056 {
		t.Fatalf("Expected repack ImageSize 31789056, got %d", res.ImageSize)
	}

	// 4. Sdat2Img back from repacked sdat
	repackImgPath := filepath.Join(tmpDir, "odm_repacked.img")
	err = sdat.Sdat2Img(filepath.Join(repackDir, "odm.transfer.list"), filepath.Join(repackDir, "odm.new.dat"), repackImgPath)
	if err != nil {
		t.Fatalf("Sdat2Img from repack failed: %v", err)
	}

	repackStat, err := os.Stat(repackImgPath)
	if err != nil {
		t.Fatalf("Stat repackImg failed: %v", err)
	}
	if repackStat.Size() != stat.Size() {
		t.Fatalf("Repacked size %d != original size %d", repackStat.Size(), stat.Size())
	}

	// 5. Compress repacked new.dat to .br
	repackBrPath := filepath.Join(repackDir, "odm.new.dat.br")
	err = brotli.CompressFile(filepath.Join(repackDir, "odm.new.dat"), repackBrPath, 6)
	if err != nil {
		t.Fatalf("CompressFile failed: %v", err)
	}
}
