// Package dyn handles Android partition layouts smoothly across the
// dynamic / non-dynamic divide.
//
// Supported container families:
//
//	dynamic-dat : dynamic_partitions_op_list + per-partition
//	              .new.dat.br/.new.dat + .transfer.list (typical custom ROM)
//	super       : loose super.img (fastboot stock ROMs)
//	payload     : payload.bin (A/B OTA / fastboot ROMs)
//	nondynamic  : per-partition .new.dat.br/.new.dat + updater-script mounts
//	raw         : loose per-partition .img files
//
// The core guarantee: Make() never mixes families silently. Every
// donor->base combination has one defined behavior:
//
//	donor dat/br  -> base dat family : direct overlay (same family)
//	donor dat/br  -> base super      : unpack donor raw -> rebuild super
//	donor dat/br  -> base raw .img   : unpack donor raw -> overlay .img
//	donor super   -> any base        : lpunpack donor -> convert per base
//	donor payload -> any base        : payload-dumper -> convert per base
//	donor raw img -> base dat family : img2sdat convert (romtool)
//	donor raw img -> base super/raw  : overlay / rebuild super
//
// Oversized results are refused (suggest GSI) instead of producing an
// unflashable or bricking image.
package dyn

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Layout is a partition container family.
type Layout string

const (
	LayoutDynamicDat Layout = "dynamic-dat"
	LayoutSuper      Layout = "super"
	LayoutPayload    Layout = "payload"
	LayoutNonDynamic Layout = "nondynamic"
	LayoutRaw        Layout = "raw"
	LayoutUnknown    Layout = "unknown"
)

// Source describes which container backs one partition in a rom_root.
type Source int

const (
	SrcMissing Source = iota
	SrcDatBr
	SrcDat
	SrcImg
	SrcSuperLogical // inside super.img, needs lpunpack
	SrcPayload      // inside payload.bin, needs payload-dumper
)

// Info is the detected layout of a rom_root.
type Info struct {
	Layout     Layout
	Groups     map[string]*Group // from op_list (dynamic-dat only)
	Sizes      map[string]int64  // partition -> resize bytes (dynamic-dat only)
	OpListPath string
	SuperPath  string
	SuperSize  int64
}

// Group is one dynamic partition group with its size budget.
type Group struct {
	Name       string
	MaxSize    int64
	Partitions []string
}

// Detect scans a rom_root and classifies its layout.
func Detect(root string) *Info {
	info := &Info{
		Layout: LayoutUnknown,
		Groups: map[string]*Group{},
		Sizes:  map[string]int64{},
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return info
	}
	names := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			names[e.Name()] = true
		}
	}
	if names["payload.bin"] {
		info.Layout = LayoutPayload
		return info
	}
	if p := filepath.Join(root, "super.img"); names["super.img"] {
		info.Layout = LayoutSuper
		info.SuperPath = p
		if st, err := os.Stat(p); err == nil {
			info.SuperSize = st.Size()
		}
		return info
	}
	if names["dynamic_partitions_op_list"] {
		info.Layout = LayoutDynamicDat
		info.OpListPath = filepath.Join(root, "dynamic_partitions_op_list")
		if op, err := ParseOpList(info.OpListPath); err == nil {
			info.Groups = op.Groups
			info.Sizes = op.Sizes
		}
		return info
	}
	hasDat := false
	hasImg := false
	for n := range names {
		if strings.HasSuffix(n, ".new.dat.br") || strings.HasSuffix(n, ".new.dat") {
			if !strings.HasSuffix(n, ".patch.dat") {
				hasDat = true
			}
		} else if strings.HasSuffix(n, ".img") {
			switch strings.TrimSuffix(n, ".img") {
			case "boot", "recovery", "vendor_boot", "init_boot", "dtbo", "vbmeta", "super_dummy", "unsparse_super_empty":
			default:
				hasImg = true
			}
		}
	}
	switch {
	case hasDat:
		info.Layout = LayoutNonDynamic
	case hasImg:
		info.Layout = LayoutRaw
	}
	return info
}

// Format reports which container backs a partition (dat + transfer.list
// count as one source; transfer.list alone is not a source).
func Format(root, name string) Source {
	if fileExists(filepath.Join(root, name+".new.dat.br")) {
		return SrcDatBr
	}
	if fileExists(filepath.Join(root, name+".new.dat")) {
		return SrcDat
	}
	if fileExists(filepath.Join(root, name+".img")) {
		return SrcImg
	}
	if fileExists(filepath.Join(root, "super.img")) {
		return SrcSuperLogical
	}
	if fileExists(filepath.Join(root, "payload.bin")) {
		return SrcPayload
	}
	return SrcMissing
}

// NeedsTransferList reports whether a dat container lacks its transfer.list.
func NeedsTransferList(root, name string) bool {
	if Format(root, name) != SrcDat && Format(root, name) != SrcDatBr {
		return false
	}
	return !fileExists(filepath.Join(root, name+".transfer.list"))
}

// ---- op_list ----

// OpList is a parsed dynamic_partitions_op_list.
type OpList struct {
	Path   string
	Groups map[string]*Group
	Sizes  map[string]int64 // partition -> resize bytes
	Lines  []string
}

// ParseOpList parses a dynamic_partitions_op_list file.
func ParseOpList(path string) (*OpList, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	op := &OpList{Path: path, Groups: map[string]*Group{}, Sizes: map[string]int64{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		raw := sc.Text()
		op.Lines = append(op.Lines, raw)
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "add_group":
			if len(fields) >= 3 {
				maxSize, _ := strconv.ParseInt(fields[2], 10, 64)
				op.Groups[fields[1]] = &Group{Name: fields[1], MaxSize: maxSize}
			}
		case "add":
			if len(fields) >= 3 {
				if g, ok := op.Groups[fields[2]]; ok {
					g.Partitions = append(g.Partitions, fields[1])
				} else {
					op.Groups[fields[2]] = &Group{Name: fields[2], Partitions: []string{fields[1]}}
				}
			}
		case "resize", "resize_partition":
			if len(fields) >= 3 {
				sz, _ := strconv.ParseInt(fields[2], 10, 64)
				op.Sizes[fields[1]] = sz
			}
		}
	}
	return op, sc.Err()
}

// SetSize updates a partition size, refusing group-budget overflows.
// This is the anti-brick gate: an oversized super image either fails to
// flash or, worse, truncates silently on some bootloaders.
func (op *OpList) SetSize(name string, size int64) error {
	if size <= 0 {
		return fmt.Errorf("invalid size %d for %s", size, name)
	}
	old := op.Sizes[name]
	op.Sizes[name] = size
	for _, g := range op.Groups {
		if !contains(g.Partitions, name) {
			continue
		}
		var total int64
		for _, p := range g.Partitions {
			total += op.Sizes[p]
		}
		if g.MaxSize > 0 && total > g.MaxSize {
			op.Sizes[name] = old
			return fmt.Errorf("group %s budget exceeded: %d > max %d (over by %d) — donor %s too big for target super, use GSI fallback",
				g.Name, total, g.MaxSize, total-g.MaxSize, name)
		}
	}
	updated := false
	for i, line := range op.Lines {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 3 && (f[0] == "resize" || f[0] == "resize_partition") && f[1] == name {
			op.Lines[i] = fmt.Sprintf("resize %s %d", name, size)
			updated = true
			break
		}
	}
	if !updated {
		op.Lines = append(op.Lines, fmt.Sprintf("resize %s %d", name, size))
	}
	return nil
}

// EnsurePartition adds a missing partition to the first group with budget.
func (op *OpList) EnsurePartition(name string, size int64) error {
	for _, g := range op.Groups {
		if contains(g.Partitions, name) {
			return op.SetSize(name, size)
		}
	}
	// Find any group with room.
	for _, g := range op.Groups {
		var total int64
		for _, p := range g.Partitions {
			total += op.Sizes[p]
		}
		if g.MaxSize <= 0 || total+size <= g.MaxSize {
			g.Partitions = append(g.Partitions, name)
			op.Lines = append(op.Lines, fmt.Sprintf("add %s %s", name, g.Name))
			return op.SetSize(name, size)
		}
	}
	return fmt.Errorf("no group has room for %s (%d bytes) — use GSI fallback", name, size)
}

// GroupUsage returns used/max bytes per group for reporting.
func (op *OpList) GroupUsage() map[string][2]int64 {
	out := map[string][2]int64{}
	for _, g := range op.Groups {
		var total int64
		for _, p := range g.Partitions {
			total += op.Sizes[p]
		}
		out[g.Name] = [2]int64{total, g.MaxSize}
	}
	return out
}

// Save writes the op_list back.
func (op *OpList) Save() error {
	return os.WriteFile(op.Path, []byte(strings.Join(op.Lines, "\n")+"\n"), 0644)
}

// ---- image validation ----

// ImageKind classifies a raw image by magic.
type ImageKind string

const (
	KindExt4   ImageKind = "ext4"
	KindErofs  ImageKind = "erofs"
	KindSparse ImageKind = "sparse"
	KindEmpty  ImageKind = "empty"
	KindBad    ImageKind = "bad"
)

// ValidateImage checks a raw .img is non-empty and has a known FS magic.
// minSize guards against truncated/corrupt downloads producing bad flashes.
func ValidateImage(path string, minSize int64) (ImageKind, error) {
	st, err := os.Stat(path)
	if err != nil {
		return KindBad, fmt.Errorf("missing %s: %w", path, err)
	}
	if st.Size() == 0 {
		return KindEmpty, fmt.Errorf("%s is 0 bytes — corrupt/truncated, refusing", filepath.Base(path))
	}
	if minSize > 0 && st.Size() < minSize {
		return KindBad, fmt.Errorf("%s is %d bytes (min %d) — truncated, refusing", filepath.Base(path), st.Size(), minSize)
	}
	f, err := os.Open(path)
	if err != nil {
		return KindBad, err
	}
	defer f.Close()
	hdr := make([]byte, 1100)
	n, err := io.ReadFull(f, hdr)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return KindBad, fmt.Errorf("%s unreadable: %w", filepath.Base(path), err)
	}
	if n < 16 {
		return KindBad, fmt.Errorf("%s too small for any filesystem", filepath.Base(path))
	}
	le32 := func(o int) uint32 {
		return uint32(hdr[o]) | uint32(hdr[o+1])<<8 | uint32(hdr[o+2])<<16 | uint32(hdr[o+3])<<24
	}
	if le32(0) == 0xed26ff3a {
		return KindSparse, nil
	}
	if n >= 1028 && le32(1024) == 0xe0f5e1e2 {
		return KindErofs, nil
	}
	if n >= 1082 && hdr[1080] == 0x53 && hdr[1081] == 0xef {
		return KindExt4, nil
	}
	return KindBad, fmt.Errorf("%s has unknown magic — not ext4/erofs/sparse, refusing", filepath.Base(path))
}

// ValidateSuper checks a super.img is non-empty and carries LP metadata
// magic ("0PLA" at offset 12288). Guards against truncated downloads and
// wrong-file mixups before lpunpack/lpmake.
func ValidateSuper(path string, minSize int64) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("missing %s: %w", filepath.Base(path), err)
	}
	if st.Size() == 0 {
		return fmt.Errorf("%s is 0 bytes — corrupt/truncated, refusing", filepath.Base(path))
	}
	if minSize > 0 && st.Size() < minSize {
		return fmt.Errorf("%s is %d bytes (min %d) — truncated, refusing", filepath.Base(path), st.Size(), minSize)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := f.ReadAt(magic, 12288); err != nil {
		return fmt.Errorf("%s too small for LP metadata — refusing", filepath.Base(path))
	}
	if magic[0] != 0x30 || magic[1] != 0x50 || magic[2] != 0x4c || magic[3] != 0x41 {
		// Sparse-wrapped super: unsparse first, then re-check.
		hdr := make([]byte, 4)
		if _, err := f.ReadAt(hdr, 0); err == nil &&
			hdr[0] == 0x3a && hdr[1] == 0xff && hdr[2] == 0x26 && hdr[3] == 0xed {
			return nil
		}
		return fmt.Errorf("%s has no LP metadata magic — not a super image, refusing", filepath.Base(path))
	}
	return nil
}

// ValidateBootImg checks boot/vendor_boot magic (ANDROID! / VNDRBOOT).
func ValidateBootImg(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("missing %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	magic := make([]byte, 8)
	if n, _ := f.Read(magic); n < 8 {
		return fmt.Errorf("%s truncated", filepath.Base(path))
	}
	if string(magic) != "ANDROID!" && string(magic) != "VNDRBOOT" {
		return fmt.Errorf("%s bad magic %q — not a boot image, refusing", filepath.Base(path), string(magic))
	}
	return nil
}

// TransferEntry is one parsed transfer-list command.
type TransferEntry struct {
	Cmd    string
	Ranges [][2]int64 // [begin,end) pairs in blocks
}

// ParseTransferList reads a v4 transfer.list.
// total is line 2 (blocks to write from new.dat), eraseEnd is the highest block in erase commands.
func ParseTransferList(path string) (total int64, eraseEnd int64, cmds []TransferEntry, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, nil, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 4 {
		return 0, 0, nil, fmt.Errorf("transfer.list too short")
	}
	total, err = strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("bad total line: %w", err)
	}
	for _, ln := range lines[2:] {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		nums := strings.Split(fields[1], ",")
		var ranges [][2]int64
		for i := 1; i+1 < len(nums); i += 2 {
			b, err1 := strconv.ParseInt(nums[i], 10, 64)
			e1, err2 := strconv.ParseInt(nums[i+1], 10, 64)
			if err1 != nil || err2 != nil {
				continue
			}
			ranges = append(ranges, [2]int64{b, e1})
			if fields[0] == "erase" && e1 > eraseEnd {
				eraseEnd = e1
			}
		}
		cmds = append(cmds, TransferEntry{Cmd: fields[0], Ranges: ranges})
	}
	return total, eraseEnd, cmds, nil
}

// MaxRangeEnd returns the highest block referenced by new/erase commands.
func MaxRangeEnd(cmds []TransferEntry) int64 {
	var maxVal int64
	for _, c := range cmds {
		if c.Cmd != "new" && c.Cmd != "erase" && c.Cmd != "zero" {
			continue
		}
		for _, r := range c.Ranges {
			if r[1] > maxVal {
				maxVal = r[1]
			}
		}
	}
	return maxVal
}

// CheckTransferListCapacity verifies that commands in transfer.list do not
// exceed the physical block capacity of the target device partition.
// Writing beyond partition boundary aborts with E1001/E2001 in recovery.
func CheckTransferListCapacity(path string, maxAllowedBlocks int64) error {
	_, eraseEnd, cmds, err := ParseTransferList(path)
	if err != nil {
		return err
	}
	maxEnd := MaxRangeEnd(cmds)
	if eraseEnd > maxEnd {
		maxEnd = eraseEnd
	}
	if maxAllowedBlocks > 0 && maxEnd > maxAllowedBlocks {
		return fmt.Errorf("%s addresses block %d, which exceeds target device partition capacity %d blocks (%d bytes) — recovery will abort with E1001/E2001",
			filepath.Base(path), maxEnd, maxAllowedBlocks, maxAllowedBlocks*4096)
	}
	return nil
}


// CheckTransferList verifies a transfer.list is self-consistent: the total
// (line 2) must cover every referenced block. romtool's img2sdat writes the
// data-block count as the total instead of image blocks — recovery then
// aborts with E1001/E2001. See FixTransferList.
func CheckTransferList(path string) error {
	total, eraseEnd, cmds, err := ParseTransferList(path)
	if err != nil {
		return err
	}
	want := eraseEnd
	if m := MaxRangeEnd(cmds); m > want {
		want = m
	}
	if want > total {
		return fmt.Errorf("%s: total %d blocks but commands address block %d — malformed, recovery will abort (E1001). Correct total is %d",
			filepath.Base(path), total, want, want)
	}
	return nil
}

// FixTransferList rewrites a malformed total to cover all referenced
// blocks (max of erase end and command ranges). Returns true if changed.
func FixTransferList(path string) (bool, error) {
	total, eraseEnd, cmds, err := ParseTransferList(path)
	if err != nil {
		return false, err
	}
	want := eraseEnd
	if m := MaxRangeEnd(cmds); m > want {
		want = m
	}
	if want <= total {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(data), "\n")
	lines[1] = strconv.FormatInt(want, 10)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return false, err
	}
	return true, nil
}

// FixTransferLists repairs every *.transfer.list under romRoot.
func FixTransferLists(romRoot string) ([]string, error) {
	entries, err := os.ReadDir(romRoot)
	if err != nil {
		return nil, err
	}
	var fixed []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".transfer.list") {
			continue
		}
		p := filepath.Join(romRoot, e.Name())
		changed, err := FixTransferList(p)
		if err != nil {
			return fixed, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if changed {
			fixed = append(fixed, e.Name())
		}
	}
	return fixed, nil
}

// ValidateDatPair ensures a dat container has its transfer.list and both
// are non-empty. A dat without transfer.list flashes garbage.
func ValidateDatPair(root, name string) error {
	dat := ""
	if fileExists(filepath.Join(root, name+".new.dat.br")) {
		dat = filepath.Join(root, name+".new.dat.br")
	} else if fileExists(filepath.Join(root, name+".new.dat")) {
		dat = filepath.Join(root, name+".new.dat")
	} else {
		return fmt.Errorf("no dat container for %s", name)
	}
	st, err := os.Stat(dat)
	if err != nil || st.Size() == 0 {
		return fmt.Errorf("%s empty/missing — refusing", filepath.Base(dat))
	}
	tl := filepath.Join(root, name+".transfer.list")
	tst, err := os.Stat(tl)
	if err != nil || tst.Size() == 0 {
		return fmt.Errorf("%s has dat but no usable transfer.list — refusing (would flash garbage)", name)
	}
	return nil
}

func contains(in []string, s string) bool {
	for _, x := range in {
		if x == s {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
