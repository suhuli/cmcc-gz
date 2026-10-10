//go:build !windows

package config

import "os"

func replaceFile(src, dst string) error { return os.Rename(src, dst) }

func restrictToOwner(path string) { _ = os.Chmod(path, 0o600) }
