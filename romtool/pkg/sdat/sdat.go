package sdat

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const BlockSize = 4096

type Range struct {
	Begin int64
	End   int64
}

func (r Range) Blocks() int64 {
	return r.End - r.Begin
}

func ParseRangeSet(s string) ([]Range, error) {
	parts := strings.Split(strings.TrimSpace(s), ",")
	if len(parts) < 3 {
		return nil, fmt.Errorf("invalid range set: %s", s)
	}

	count, err := strconv.Atoi(parts[0])
	if err != nil || count%2 != 0 || len(parts) != count+1 {
		return nil, fmt.Errorf("malformed range set format: %s", s)
	}

	ranges := make([]Range, 0, count/2)
	for i := 1; i < len(parts); i += 2 {
		begin, err1 := strconv.ParseInt(parts[i], 10, 64)
		end, err2 := strconv.ParseInt(parts[i+1], 10, 64)
		if err1 != nil || err2 != nil || end < begin {
			return nil, fmt.Errorf("invalid range values in: %s", s)
		}
		ranges = append(ranges, Range{Begin: begin, End: end})
	}

	return ranges, nil
}

func FormatRangeSet(ranges []Range) string {
	parts := make([]string, 0, 1+len(ranges)*2)
	parts = append(parts, strconv.Itoa(len(ranges)*2))
	for _, r := range ranges {
		parts = append(parts, strconv.FormatInt(r.Begin, 10), strconv.FormatInt(r.End, 10))
	}
	return strings.Join(parts, ",")
}

// GetMaxBlock parses a transfer list to find the highest block referenced (partition size in blocks).
func GetMaxBlock(transferListPath string) (int64, error) {
	tlFile, err := os.Open(transferListPath)
	if err != nil {
		return 0, err
	}
	defer tlFile.Close()

	scanner := bufio.NewScanner(tlFile)
	var lines []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if len(lines) < 4 {
		return 0, fmt.Errorf("transfer list too short")
	}

	var maxBlock int64 = 0
	for _, line := range lines[4:] {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			ranges, err := ParseRangeSet(fields[1])
			if err == nil {
				for _, r := range ranges {
					if r.End > maxBlock {
						maxBlock = r.End
					}
				}
			}
		}
	}
	return maxBlock, nil
}

// Sdat2Img converts Android sparse data files (.transfer.list + .new.dat) to a raw .img.
func Sdat2Img(transferListPath, newDatPath, outputImgPath string) error {
	tlFile, err := os.Open(transferListPath)
	if err != nil {
		return fmt.Errorf("failed opening transfer list %s: %w", transferListPath, err)
	}
	defer tlFile.Close()

	scanner := bufio.NewScanner(tlFile)
	var lines []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed reading transfer list: %w", err)
	}

	if len(lines) < 4 {
		return fmt.Errorf("transfer list too short: %d lines", len(lines))
	}

	// First pass: find maximum block index to size the output image
	var maxBlock int64 = 0
	for _, line := range lines[4:] {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			ranges, err := ParseRangeSet(fields[1])
			if err == nil {
				for _, r := range ranges {
					if r.End > maxBlock {
						maxBlock = r.End
					}
				}
			}
		}
	}

	if maxBlock == 0 {
		return fmt.Errorf("no valid block ranges found in transfer list")
	}

	// Create and truncate output image
	outImg, err := os.Create(outputImgPath)
	if err != nil {
		return fmt.Errorf("failed creating output image %s: %w", outputImgPath, err)
	}
	defer outImg.Close()

	totalBytes := maxBlock * BlockSize
	if err := outImg.Truncate(totalBytes); err != nil {
		return fmt.Errorf("failed allocating output image size %d bytes: %w", totalBytes, err)
	}

	// Open new.dat for streaming reads
	newDatFile, err := os.Open(newDatPath)
	if err != nil {
		return fmt.Errorf("failed opening new.dat %s: %w", newDatPath, err)
	}
	defer newDatFile.Close()

	newDatReader := bufio.NewReaderSize(newDatFile, 1024*1024)
	buffer := make([]byte, 1024*1024) // 1MB reusable buffer

	// Second pass: apply commands
	for _, line := range lines[4:] {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		cmd := fields[0]
		ranges, err := ParseRangeSet(fields[1])
		if err != nil {
			continue
		}

		switch cmd {
		case "new":
			for _, r := range ranges {
				offset := r.Begin * BlockSize
				bytesToCopy := r.Blocks() * BlockSize

				if _, err := outImg.Seek(offset, io.SeekStart); err != nil {
					return fmt.Errorf("failed seeking in output image: %w", err)
				}

				copied, err := io.CopyBuffer(outImg, io.LimitReader(newDatReader, bytesToCopy), buffer)
				if err != nil {
					return fmt.Errorf("failed reading from new.dat: %w", err)
				}
				if copied != bytesToCopy {
					return fmt.Errorf("short read from new.dat: expected %d, got %d", bytesToCopy, copied)
				}
			}

		case "zero":
			zeroChunk := make([]byte, 64*1024)
			for _, r := range ranges {
				offset := r.Begin * BlockSize
				bytesToWrite := r.Blocks() * BlockSize
				if _, err := outImg.Seek(offset, io.SeekStart); err != nil {
					return fmt.Errorf("failed seeking: %w", err)
				}
				var remaining = bytesToWrite
				for remaining > 0 {
					chunk := int64(len(zeroChunk))
					if chunk > remaining {
						chunk = remaining
					}
					if _, err := outImg.Write(zeroChunk[:chunk]); err != nil {
						return fmt.Errorf("failed writing zeros: %w", err)
					}
					remaining -= chunk
				}
			}

		case "erase":
			// Erase is handled implicitly by sparse file / truncate
		}
	}

	return nil
}

type SdatResult struct {
	TotalBlocks int64
	DataBlocks  int64
	ImageSize   int64
}

// Img2Sdat converts a raw filesystem .img to Android .transfer.list and .new.dat.
func Img2Sdat(inputImgPath, outputDir, partitionName string, maxChunkBlocks int64) (*SdatResult, error) {
	if maxChunkBlocks <= 0 {
		maxChunkBlocks = 1024 // 4MB chunks
	}

	imgFile, err := os.Open(inputImgPath)
	if err != nil {
		return nil, fmt.Errorf("failed opening image %s: %w", inputImgPath, err)
	}
	defer imgFile.Close()

	stat, err := imgFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed stating image %s: %w", inputImgPath, err)
	}

	imgSize := stat.Size()
	if imgSize%BlockSize != 0 {
		// Pad to multiple of BlockSize if necessary
		paddedSize := ((imgSize + BlockSize - 1) / BlockSize) * BlockSize
		imgSize = paddedSize
	}

	totalBlocks := imgSize / BlockSize
	if totalBlocks == 0 {
		return nil, fmt.Errorf("image %s has 0 blocks", inputImgPath)
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("failed creating output dir %s: %w", outputDir, err)
	}

	newDatPath := fmt.Sprintf("%s/%s.new.dat", outputDir, partitionName)
	tlPath := fmt.Sprintf("%s/%s.transfer.list", outputDir, partitionName)
	patchDatPath := fmt.Sprintf("%s/%s.patch.dat", outputDir, partitionName)

	newDatFile, err := os.Create(newDatPath)
	if err != nil {
		return nil, fmt.Errorf("failed creating %s: %w", newDatPath, err)
	}
	defer newDatFile.Close()

	newDatBuf := bufio.NewWriterSize(newDatFile, 1024*1024)
	defer newDatBuf.Flush()

	zeroBlock := make([]byte, BlockSize)
	currentBlock := make([]byte, BlockSize)

	var dataBlocksTotal int64 = 0
	type blockSpan struct {
		start int64
		end   int64
	}
	var spans []blockSpan
	var inSpan bool
	var spanStart int64

	bufImg := bufio.NewReaderSize(imgFile, 1024*1024)

	// Step 1: Scan for data blocks and write them to newDat
	for b := int64(0); b < totalBlocks; b++ {
		n, err := io.ReadFull(bufImg, currentBlock)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("failed reading block %d: %w", b, err)
		}
		if n < BlockSize {
			// Zero out rest of block
			for i := n; i < BlockSize; i++ {
				currentBlock[i] = 0
			}
		}

		isZero := bytes.Equal(currentBlock, zeroBlock)
		if !isZero {
			dataBlocksTotal++
			if !inSpan {
				inSpan = true
				spanStart = b
			}
			if _, err := newDatBuf.Write(currentBlock); err != nil {
				return nil, fmt.Errorf("failed writing to new.dat: %w", err)
			}
		} else {
			if inSpan {
				spans = append(spans, blockSpan{start: spanStart, end: b})
				inSpan = false
			}
		}
	}
	if inSpan {
		spans = append(spans, blockSpan{start: spanStart, end: totalBlocks})
	}

	// Flush newDat buffer
	if err := newDatBuf.Flush(); err != nil {
		return nil, fmt.Errorf("failed flushing new.dat: %w", err)
	}

	// Step 2: Split spans into maxChunkBlocks chunks
	var chunkedRanges []Range
	for _, span := range spans {
		cur := span.start
		for cur < span.end {
			next := cur + maxChunkBlocks
			if next > span.end {
				next = span.end
			}
			chunkedRanges = append(chunkedRanges, Range{Begin: cur, End: next})
			cur = next
		}
	}

	// Step 3: Write transfer.list
	tlFile, err := os.Create(tlPath)
	if err != nil {
		return nil, fmt.Errorf("failed creating %s: %w", tlPath, err)
	}
	defer tlFile.Close()

	tlWriter := bufio.NewWriter(tlFile)
	defer tlWriter.Flush()

	// Header:
	// Version 4
	// Total blocks to write
	// 0 (max stash)
	// 0 (stash blocks)
	fmt.Fprintf(tlWriter, "4\n%d\n0\n0\n", dataBlocksTotal)

	// Erase full partition range first
	fmt.Fprintf(tlWriter, "erase 2,0,%d\n", totalBlocks)

	// Emit new commands
	for _, r := range chunkedRanges {
		fmt.Fprintf(tlWriter, "new 2,%d,%d\n", r.Begin, r.End)
	}

	// Create empty patch.dat
	if err := os.WriteFile(patchDatPath, []byte{}, 0644); err != nil {
		return nil, fmt.Errorf("failed creating patch.dat: %w", err)
	}

	return &SdatResult{
		TotalBlocks: totalBlocks,
		DataBlocks:  dataBlocksTotal,
		ImageSize:   totalBlocks * BlockSize,
	}, nil
}
