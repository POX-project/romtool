package dynamic

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type DynamicGroup struct {
	Name       string   `json:"name"`
	MaxSize    int64    `json:"max_size"`
	Partitions []string `json:"partitions"`
}

type DynamicConfig struct {
	FilePath   string                   `json:"file_path"`
	Groups     map[string]*DynamicGroup `json:"groups"`
	Partitions map[string]int64         `json:"partitions"` // partition -> size in bytes
	Lines      []string                 `json:"lines"`
}

// ParseOpList parses a dynamic_partitions_op_list file.
func ParseOpList(path string) (*DynamicConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed opening dynamic partitions op list %s: %w", path, err)
	}
	defer file.Close()

	cfg := &DynamicConfig{
		FilePath:   path,
		Groups:     make(map[string]*DynamicGroup),
		Partitions: make(map[string]int64),
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		rawLine := scanner.Text()
		cfg.Lines = append(cfg.Lines, rawLine)

		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		cmd := parts[0]
		switch cmd {
		case "add_group":
			if len(parts) >= 3 {
				maxSize, _ := strconv.ParseInt(parts[2], 10, 64)
				cfg.Groups[parts[1]] = &DynamicGroup{
					Name:       parts[1],
					MaxSize:    maxSize,
					Partitions: []string{},
				}
			}
		case "add":
			if len(parts) >= 3 {
				partitionName := parts[1]
				groupName := parts[2]
				if g, ok := cfg.Groups[groupName]; ok {
					g.Partitions = append(g.Partitions, partitionName)
				}
			}
		case "resize":
			if len(parts) >= 3 {
				partitionName := parts[1]
				sz, _ := strconv.ParseInt(parts[2], 10, 64)
				cfg.Partitions[partitionName] = sz
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed reading dynamic op list: %w", err)
	}

	return cfg, nil
}

// UpdatePartitionSize updates the resize value for a partition.
func (c *DynamicConfig) UpdatePartitionSize(partitionName string, newSizeInBytes int64) error {
	c.Partitions[partitionName] = newSizeInBytes

	// Check group size limits
	for _, g := range c.Groups {
		for _, p := range g.Partitions {
			if p == partitionName {
				var total int64
				for _, part := range g.Partitions {
					total += c.Partitions[part]
				}
				if g.MaxSize > 0 && total > g.MaxSize {
					return fmt.Errorf("total size of group %s (%d bytes) exceeds max size (%d bytes) by %d bytes",
						g.Name, total, g.MaxSize, total-g.MaxSize)
				}
			}
		}
	}

	// Update in Lines
	updated := false
	for i, line := range c.Lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "resize ") {
			fields := strings.Fields(trimmed)
			if len(fields) >= 3 && fields[1] == partitionName {
				c.Lines[i] = fmt.Sprintf("resize %s %d", partitionName, newSizeInBytes)
				updated = true
				break
			}
		}
	}

	if !updated {
		c.Lines = append(c.Lines, fmt.Sprintf("resize %s %d", partitionName, newSizeInBytes))
	}

	return nil
}

// Save writes the updated lines back to FilePath.
func (c *DynamicConfig) Save() error {
	content := strings.Join(c.Lines, "\n") + "\n"
	return os.WriteFile(c.FilePath, []byte(content), 0644)
}
