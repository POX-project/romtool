package payload

import (
	"fmt"
	"os"
	"strings"

	"romtool/pkg/toolkit"
)

// ExtractPayload extracts partitions from an Android payload.bin file using payload-dumper-go.
func ExtractPayload(tk *toolkit.Toolkit, payloadPath, outputDir string, partitions []string) error {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed creating output dir %s: %w", outputDir, err)
	}

	args := []string{"-o", outputDir}
	if len(partitions) > 0 {
		args = append(args, "-p", strings.Join(partitions, ","))
	}
	args = append(args, payloadPath)

	output, err := tk.Run("payload-dumper-go", args...)
	if err != nil {
		return fmt.Errorf("failed running payload-dumper-go: %w (output: %s)", err, output)
	}

	return nil
}
