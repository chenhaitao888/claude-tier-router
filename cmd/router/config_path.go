package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Resolve the executable symlink before searching for its configuration. The
// installed command points at <checkout>/bin/router, so neither the caller's
// working directory nor the database location needs to change.
func defaultConfigPath(explicit, executable, cwd, home string) (string, error) {
	abs := func(path string) string {
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Join(cwd, path)
	}
	if explicit != "" {
		return abs(explicit), nil
	}
	var candidates []string
	if executable != "" {
		resolved, err := filepath.EvalSymlinks(abs(executable))
		if err != nil {
			return "", fmt.Errorf("resolve router executable: %w", err)
		}
		dir := filepath.Dir(resolved)
		if filepath.Base(dir) == "bin" {
			candidates = append(candidates, filepath.Join(filepath.Dir(dir), "config.json"))
		}
		candidates = append(candidates, filepath.Join(dir, "config.json"))
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "claude-tier-router", "config.json"))
	}
	// Retain the old local configuration behavior, including go run, as a fallback.
	candidates = append(candidates, filepath.Join(cwd, "config.json"))
	for _, path := range candidates {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read configuration %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("configuration must be a regular file: %s", path)
		}
		return path, nil
	}
	return "", errors.New("configuration not found: create config.json in the router checkout or ~/.config/claude-tier-router/, or use router claude -config /path/to/config.json")
}

func resolveConfigPath(explicit string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// An explicit path works even when executable or home discovery is unavailable.
	if explicit != "" {
		return defaultConfigPath(explicit, "", cwd, "")
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	return defaultConfigPath("", executable, cwd, home)
}
