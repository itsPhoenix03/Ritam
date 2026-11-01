//go:build windows

package config

import (
	"strings"

	"golang.org/x/sys/windows"
)

// resolveLinks returns the final path of an existing file or folder, every
// symbolic link and junction followed. filepath.EvalSymlinks does not follow
// junctions on Windows, so the handle is asked for its final path instead.
func resolveLinks(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	// FILE_FLAG_BACKUP_SEMANTICS is what lets a folder be opened at all.
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]uint16, n)
	}
	s := windows.UTF16ToString(buf)
	switch {
	case strings.HasPrefix(s, `\\?\UNC\`):
		return `\\` + s[len(`\\?\UNC\`):], nil
	case strings.HasPrefix(s, `\\?\`):
		return s[len(`\\?\`):], nil
	}
	return s, nil
}
