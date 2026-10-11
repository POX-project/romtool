package dyn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// writeRawImg creates a sparse file with a valid filesystem magic.
func writeRawImg(t *testing.T, path, kind string, size int64) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 1100)
	switch kind {
	case "sparse":
		hdr[0], hdr[1], hdr[2], hdr[3] = 0x3a, 0xff, 0x26, 0xed
	case "erofs":
		hdr[1024], hdr[1025], hdr[1026], hdr[1027] = 0xe2, 0xe1, 0xf5, 0xe0
	case "ext4":
		hdr[1080], hdr[1081] = 0x53, 0xef
	}
	if _, err := f.WriteAt(hdr, 0); err != nil {
		t.Fatal(err)
	}
}

func TestParseOpListRoundtrip(t *testing.T) {
	dir := t.TempDir()
	opPath := filepath.Join(dir, "dynamic_partitions_op_list")
	writeFile(t, opPath, "# comment\nadd_group qti_dynamic_partitions 8589934592\nadd system qti_dynamic_partitions\nadd vendor qti_dynamic_partitions\nresize system 3221225472\nresize vendor 1073741824\n")
	op, err := ParseOpList(opPath)
	if err != nil {
		t.Fatal(err)
	}
	if op.Groups["qti_dynamic_partitions"].MaxSize != 8589934592 {
		t.Fatalf("bad max: %+v", op.Groups["qti_dynamic_partitions"])
	}
	if op.Sizes["system"] != 3221225472 {
		t.Fatalf("bad size: %v", op.Sizes)
	}
	if err := op.SetSize("system", 3221225472+4096); err != nil {
		t.Fatal(err)
	}
	if err := op.Save(); err != nil {
		t.Fatal(err)
	}
	op2, err := ParseOpList(opPath)
	if err != nil {
		t.Fatal(err)
	}
	if op2.Sizes["system"] != 3221225472+4096 {
		t.Fatalf("size not persisted: %v", op2.Sizes)
	}
}

func TestSetSizeRefusesOverflow(t *testing.T) {
	dir := t.TempDir()
	opPath := filepath.Join(dir, "dynamic_partitions_op_list")
	writeFile(t, opPath, "add_group g 1000\nadd a g\nadd b g\nresize a 600\nresize b 300\n")
	op, err := ParseOpList(opPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.SetSize("b", 500); err == nil {
		t.Fatal("expected budget refusal, got nil")
	} else if !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("wrong error: %v", err)
	}
	// Failed update must not mutate sizes.
	if op.Sizes["b"] != 300 {
		t.Fatalf("size mutated on failed update: %v", op.Sizes)
	}
}

func TestSetSizeRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	opPath := filepath.Join(dir, "dynamic_partitions_op_list")
	writeFile(t, opPath, "add_group g 1000\nadd a g\nresize a 100\n")
	op, _ := ParseOpList(opPath)
	if err := op.SetSize("a", 0); err == nil {
		t.Fatal("expected error for size 0")
	}
	if err := op.SetSize("a", -5); err == nil {
		t.Fatal("expected error for negative size")
	}
}

func TestDetectMatrix(t *testing.T) {
	mk := func(files map[string]string) string {
		dir := t.TempDir()
		for name, content := range files {
			writeFile(t, filepath.Join(dir, name), content)
		}
		return dir
	}
	cases := map[string]struct {
		files  map[string]string
		layout Layout
	}{
		"dynamic-dat": {map[string]string{"dynamic_partitions_op_list": "add_group g 100\n", "system.new.dat.br": "x", "system.transfer.list": "y"}, LayoutDynamicDat},
		"super":       {map[string]string{"super.img": "0123456789abcdef"}, LayoutSuper},
		"payload":     {map[string]string{"payload.bin": "x"}, LayoutPayload},
		"nondynamic":  {map[string]string{"system.new.dat.br": "x", "system.transfer.list": "y", "boot.img": "ANDROID!xx"}, LayoutNonDynamic},
		"raw":         {map[string]string{"system.img": "x", "boot.img": "ANDROID!xx"}, LayoutRaw},
		"unknown":     {map[string]string{"README": "hi"}, LayoutUnknown},
	}
	for name, tc := range cases {
		if got := Detect(mk(tc.files)).Layout; got != tc.layout {
			t.Errorf("%s: got %q want %q", name, got, tc.layout)
		}
	}
}

func TestFormatAndTransferList(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "system.new.dat.br"), "x")
	if Format(dir, "system") != SrcDatBr {
		t.Fatal("want SrcDatBr")
	}
	if !NeedsTransferList(dir, "system") {
		t.Fatal("want missing transfer.list reported")
	}
	writeFile(t, filepath.Join(dir, "system.transfer.list"), "y")
	if NeedsTransferList(dir, "system") {
		t.Fatal("transfer.list now present")
	}
}

func TestValidateImage(t *testing.T) {
	dir := t.TempDir()
	for _, kind := range []string{"ext4", "erofs", "sparse"} {
		p := filepath.Join(dir, kind+".img")
		writeRawImg(t, p, kind, 2<<20)
		if got, err := ValidateImage(p, 1<<20); err != nil || string(got) != kind {
			t.Errorf("%s: got %v err %v", kind, got, err)
		}
	}
	empty := filepath.Join(dir, "empty.img")
	os.WriteFile(empty, nil, 0644)
	if _, err := ValidateImage(empty, 0); err == nil {
		t.Error("empty image must be refused")
	}
	bad := filepath.Join(dir, "bad.img")
	writeFile(t, bad, strings.Repeat("Z", 2048))
	if _, err := ValidateImage(bad, 0); err == nil {
		t.Error("unknown magic must be refused")
	}
	tiny := filepath.Join(dir, "tiny.img")
	writeRawImg(t, tiny, "ext4", 100)
	if _, err := ValidateImage(tiny, 1<<20); err == nil {
		t.Error("undersized image must be refused")
	}
}

func TestValidateSuper(t *testing.T) {
	// Real super built by the roundtrip test tools is covered in engine;
	// here check the refusal paths.
	dir := t.TempDir()
	empty := filepath.Join(dir, "super.img")
	os.WriteFile(empty, nil, 0644)
	if err := ValidateSuper(empty, 0); err == nil {
		t.Error("empty super must be refused")
	}
	bad := filepath.Join(dir, "bad.img")
	f, _ := os.Create(bad)
	f.Truncate(2 << 20)
	f.WriteAt([]byte("ANDROID!notsuper...."), 0)
	f.Close()
	if err := ValidateSuper(bad, 0); err == nil {
		t.Error("non-super magic must be refused")
	}
}

func TestValidateBootImg(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "boot.img")
	writeFile(t, good, "ANDROID!payload...")
	if err := ValidateBootImg(good); err != nil {
		t.Error(err)
	}
	vendor := filepath.Join(dir, "vendor_boot.img")
	writeFile(t, vendor, "VNDRBOOTpayload")
	if err := ValidateBootImg(vendor); err != nil {
		t.Error(err)
	}
	bad := filepath.Join(dir, "bad.img")
	writeFile(t, bad, "NOTABOOTIMAGE....")
	if err := ValidateBootImg(bad); err == nil {
		t.Error("bad magic must be refused")
	}
}

func TestValidateDatPair(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "system.new.dat.br"), "datadata")
	if err := ValidateDatPair(dir, "system"); err == nil {
		t.Error("dat without transfer.list must be refused")
	}
	writeFile(t, filepath.Join(dir, "system.transfer.list"), "list")
	if err := ValidateDatPair(dir, "system"); err != nil {
		t.Error(err)
	}
}

func TestTransferListCheckAndFix(t *testing.T) {
	dir := t.TempDir()
	// Malformed: total (data blocks) smaller than addressed ranges.
	bad := "4\n100\n0\n0\nerase 2,0,500\nnew 2,0,60\nnew 2,400,500\n"
	p := filepath.Join(dir, "system.transfer.list")
	writeFile(t, p, bad)
	if err := CheckTransferList(p); err == nil {
		t.Fatal("short total must be refused (E1001 guard)")
	}
	changed, err := FixTransferList(p)
	if err != nil || !changed {
		t.Fatalf("fix failed: changed=%v err=%v", changed, err)
	}
	if err := CheckTransferList(p); err != nil {
		t.Fatalf("fixed list must pass: %v", err)
	}
	data, _ := os.ReadFile(p)
	lines := strings.Split(string(data), "\n")
	if lines[1] != "500" {
		t.Fatalf("total should be 500, got %q", lines[1])
	}
	// Already-correct list: no change.
	changed, err = FixTransferList(p)
	if err != nil || changed {
		t.Fatalf("correct list must be untouched: changed=%v err=%v", changed, err)
	}
}
