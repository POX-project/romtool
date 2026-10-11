package toolkit

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type Toolkit struct {
	BinDir string
}

type ToolStatus struct {
	Name      string
	Path      string
	Available bool
	Version   string
}

// FindToolkit locates the toolkit directory.
// Priority:
// 1. Explicit path parameter or $ROMTOOL_TOOLKIT env
// 2. Adjacent to binary: <exe_dir>/toolkit/bin or <exe_dir>/../toolkit/bin
// 3. Current working directory: ./toolkit/bin
// 4. Default fallback: /mnt/.../toolkit/bin
func FindToolkit(customPath string) *Toolkit {
	candidates := []string{}

	if customPath != "" {
		candidates = append(candidates, customPath)
	}

	if env := os.Getenv("ROMTOOL_TOOLKIT"); env != "" {
		candidates = append(candidates, env)
	}

	exePath, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exePath)
		candidates = append(candidates,
			filepath.Join(exeDir, "toolkit", "bin"),
			filepath.Join(exeDir, "..", "toolkit", "bin"),
			filepath.Join(exeDir, "bin"),
		)
	}

	cwd, err := os.Getwd()
	if err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "toolkit", "bin"),
			filepath.Join(cwd, "..", "toolkit", "bin"),
		)
	}

	// Known workspace directory fallback
	candidates = append(candidates,
		"/mnt/9f4d8733-a954-435d-b92f-f33c068c3b1e/repack bego/toolkit/bin",
	)

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return &Toolkit{BinDir: c}
		}
	}

	return &Toolkit{BinDir: ""}
}

// GetToolPath returns the executable path for the requested tool.
// If found in the toolkit BinDir, that path is returned. Otherwise, checks system PATH.
func (t *Toolkit) GetToolPath(name string) (string, error) {
	if t.BinDir != "" {
		candidate := filepath.Join(t.BinDir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			// Ensure it has execute permissions
			_ = os.Chmod(candidate, 0755)
			return candidate, nil
		}
	}

	// Fallback to system PATH
	path, err := exec.LookPath(name)
	if err == nil {
		return path, nil
	}

	return "", fmt.Errorf("tool %q not found in toolkit (%s) or system PATH", name, t.BinDir)
}

// Run executes a tool and returns stdout+stderr combined output.
func (t *Toolkit) Run(name string, args ...string) (string, error) {
	return t.RunInDir("", name, args...)
}

// RunInDir executes a tool in a specific directory.
func (t *Toolkit) RunInDir(dir, name string, args ...string) (string, error) {
	binPath, err := t.GetToolPath(name)
	if err != nil {
		return "", err
	}

	cmdArgs := append([]string{"-n", "19", "ionice", "-c", "3", binPath}, args...)
	cmd := exec.Command("nice", cmdArgs...)
	if dir != "" {
		cmd.Dir = dir
	}

	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	err = cmd.Run()
	output := outBuf.String()
	if err != nil {
		return output, fmt.Errorf("failed running %s %v: %w (output: %s)", name, args, err, output)
	}

	return output, nil
}

// Has returns true if the tool exists and is executable.
func (t *Toolkit) Has(name string) bool {
	_, err := t.GetToolPath(name)
	return err == nil
}

// CheckTools inspects all key Android tools and returns their statuses.
func (t *Toolkit) CheckTools() map[string]ToolStatus {
	requiredTools := []string{
		"magiskboot",
		"mkfs.erofs",
		"fsck.erofs",
		"make_ext4fs",
		"lpmake",
		"lpunpack",
		"simg2img",
		"img2simg",
		"zipalign",
		"7z",
		"avbtool",
	}

	results := make(map[string]ToolStatus)
	for _, name := range requiredTools {
		path, err := t.GetToolPath(name)
		status := ToolStatus{
			Name:      name,
			Available: err == nil,
			Path:      path,
		}
		if err == nil {
			// Quick test version/help
			cmd := exec.Command(path)
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			_ = cmd.Run()
			line := ""
			for _, l := range bytes.Split(out.Bytes(), []byte("\n")) {
				trimmed := string(bytes.TrimSpace(l))
				if trimmed != "" {
					line = trimmed
					break
				}
			}
			status.Version = line
		}
		results[name] = status
	}
	return results
}
