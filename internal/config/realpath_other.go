//go:build !windows

package config

import "path/filepath"

// resolveLinks returns the final path of an existing file or folder, every
// symbolic link followed.
func resolveLinks(path string) (string, error) { return filepath.EvalSymlinks(path) }
