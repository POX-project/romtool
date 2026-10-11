package brotli

import (
	"bufio"
	"fmt"
	"io"
	"os"

	cbrotli "github.com/andybalholm/brotli"
)

// DecompressFile decompresses a .br file to an output file.
func DecompressFile(srcPath, dstPath string) error {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("failed opening source file %s: %w", srcPath, err)
	}
	defer srcFile.Close()

	bufSrc := bufio.NewReaderSize(srcFile, 1024*1024)
	brReader := cbrotli.NewReader(bufSrc)

	dstFile, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("failed creating target file %s: %w", dstPath, err)
	}
	defer dstFile.Close()

	bufDst := bufio.NewWriterSize(dstFile, 1024*1024)
	defer bufDst.Flush()

	buf := make([]byte, 1024*1024) // 1MB chunk buffer
	if _, err := io.CopyBuffer(bufDst, brReader, buf); err != nil {
		return fmt.Errorf("brotli decompression failed: %w", err)
	}

	return nil
}

// CompressFile compresses a file to .br format.
// quality can be between 0 (fastest) and 11 (maximum). Recommended: 6 for balanced speed/size, 9 for release.
func CompressFile(srcPath, dstPath string, quality int) error {
	if quality < 0 || quality > 11 {
		quality = 6
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("failed opening source file %s: %w", srcPath, err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("failed creating target file %s: %w", dstPath, err)
	}
	defer dstFile.Close()

	bufSrc := bufio.NewReaderSize(srcFile, 1024*1024)
	bufDst := bufio.NewWriterSize(dstFile, 1024*1024)
	defer bufDst.Flush()

	brWriter := cbrotli.NewWriterLevel(bufDst, quality)
	defer brWriter.Close()

	buf := make([]byte, 1024*1024)
	if _, err := io.CopyBuffer(brWriter, bufSrc, buf); err != nil {
		return fmt.Errorf("brotli compression failed: %w", err)
	}

	return nil
}
