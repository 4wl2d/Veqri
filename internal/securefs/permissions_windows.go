//go:build windows

package securefs

import (
	"io/fs"
	"os"
)

func validateMode(fs.FileMode, fs.FileMode) error {
	// Windows' os.Chmod controls only the read-only attribute. Restricted DACLs
	// are installed and verified by the Windows service tooling.
	return nil
}

func openPrivateFile(path string, flag int, perm fs.FileMode) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrInvalid}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return os.OpenFile(path, flag, perm)
}

func openPrivateDirectory(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrInvalid}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return os.Open(path)
}

func syncDirectory(path string) error {
	// Go does not expose a portable Windows directory FlushFileBuffers handle.
	// Open and validate without following an ordinary symlink, then close.
	directory, err := openPrivateDirectory(path)
	if err != nil {
		return err
	}
	info, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		return err
	}
	if !info.IsDir() {
		_ = directory.Close()
		return os.ErrInvalid
	}
	return directory.Close()
}
