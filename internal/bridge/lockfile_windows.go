//go:build windows

package bridge

import (
	"os"
	"syscall"
)

// openExclusiveLockFile opens path without FILE_SHARE_DELETE so another bridge
// instance scavenging temp directories cannot remove .lock while this Server is open.
func openExclusiveLockFile(path string) (*os.File, error) {
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(
		pathp,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ,
		nil,
		syscall.CREATE_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func removeLockFile(path string) error {
	return os.Remove(path)
}
