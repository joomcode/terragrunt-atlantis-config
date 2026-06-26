package cmd

// Small filesystem helpers that used to live in terragrunt's `util` package.
// As of terragrunt v1.0.x that package moved under `internal/`, so it can no
// longer be imported. These are direct equivalents of the originals.

import (
	"fmt"
	"os"
	"path/filepath"
)

func joinPath(elem ...string) string {
	return filepath.ToSlash(filepath.Join(elem...))
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readFileAsString(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("error reading file at path %s: %w", path, err)
	}
	return string(b), nil
}
