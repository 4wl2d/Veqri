//go:build !windows

package securefs

import (
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func validateMode(got, want fs.FileMode) error {
	if got.Perm() != want.Perm() {
		return fmt.Errorf("permissions are %#o, want %#o", got.Perm(), want.Perm())
	}
	return nil
}

func openPrivateFile(path string, flag int, perm fs.FileMode) (*os.File, error) {
	fd, err := unix.Open(path, flag|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("wrap private file descriptor")
	}
	return file, nil
}

func openPrivateDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	directory := os.NewFile(uintptr(fd), path)
	if directory == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("wrap private directory descriptor")
	}
	return directory, nil
}

func syncDirectory(path string) error {
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
		return fmt.Errorf("sync path is not a directory")
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}
