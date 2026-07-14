package securefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	PrivateDirMode  fs.FileMode = 0o700
	PrivateFileMode fs.FileMode = 0o600
)

// EnsurePrivateDir creates path when necessary and restricts it to the current
// user. Unix implementations also verify the exact resulting permission bits;
// Windows applies the closest os.Chmod best effort while service installers
// remain responsible for the directory DACL.
func EnsurePrivateDir(path string) error {
	if path == "" {
		return errors.New("private directory path is required")
	}
	if err := os.MkdirAll(path, PrivateDirMode); err != nil {
		return fmt.Errorf("create private directory %q: %w", path, err)
	}
	directory, err := openPrivateDirectory(path)
	if err != nil {
		return fmt.Errorf("open private directory %q: %w", path, err)
	}
	if err := enforceDirectoryMode(directory, PrivateDirMode); err != nil {
		_ = directory.Close()
		return fmt.Errorf("secure private directory %q: %w", path, err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close private directory %q: %w", path, err)
	}
	return nil
}

// EnsurePrivateDirDurable creates or repairs path, syncs the directory itself,
// and then syncs its parent. The parent sync makes a newly created directory
// entry crash-durable before callers begin publishing private artifacts in it.
func EnsurePrivateDirDurable(path string) error {
	if err := EnsurePrivateDir(path); err != nil {
		return err
	}
	if err := SyncDir(path); err != nil {
		return fmt.Errorf("sync private directory %q: %w", path, err)
	}
	parent := filepath.Dir(filepath.Clean(path))
	if err := SyncDir(parent); err != nil {
		return fmt.Errorf("sync parent of private directory %q: %w", path, err)
	}
	return nil
}

// EnsurePrivateFile restricts an existing regular file to the current user.
func EnsurePrivateFile(path string) error {
	if path == "" {
		return errors.New("private file path is required")
	}
	file, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open private file %q: %w", path, err)
	}
	if err := enforceFileMode(file, PrivateFileMode); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure private file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private file %q: %w", path, err)
	}
	return nil
}

// EnsurePrivateFileIfExists secures path when present and otherwise succeeds.
func EnsurePrivateFileIfExists(path string) error {
	err := EnsurePrivateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// EnsurePrivateTreeIfExists repairs an application-owned artifact directory
// and every directory/file below it. Missing trees are left missing. Symlinks
// and other non-regular entries fail closed instead of redirecting chmod or
// read/write operations outside the private tree.
func EnsurePrivateTreeIfExists(root string) error {
	if root == "" {
		return errors.New("private tree path is required")
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect private tree %q: %w", root, err)
	}
	if err := EnsurePrivateDir(root); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("private tree entry %q is a symlink", path)
		}
		if entry.IsDir() {
			return EnsurePrivateDir(path)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect private tree entry %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("private tree entry %q is not a regular file", path)
		}
		if err := EnsurePrivateFile(path); err != nil {
			return fmt.Errorf("secure private tree entry %q: %w", path, err)
		}
		return nil
	})
}

// CreateOrSecurePrivateFile creates path without truncation when it does not
// exist, or repairs the permissions of the existing regular file.
func CreateOrSecurePrivateFile(path string) error {
	if path == "" {
		return errors.New("private file path is required")
	}
	file, err := openPrivateFile(path, os.O_RDWR|os.O_CREATE, PrivateFileMode)
	if err != nil {
		return fmt.Errorf("open private file %q: %w", path, err)
	}
	if err := enforceFileMode(file, PrivateFileMode); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure private file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private file %q: %w", path, err)
	}
	return nil
}

// ReadPrivateFile secures and reads the same no-follow file descriptor.
func ReadPrivateFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("private file path is required")
	}
	file, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open private file %q: %w", path, err)
	}
	if err := enforceFileMode(file, PrivateFileMode); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure private file %q: %w", path, err)
	}
	contents, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read private file %q: %w", path, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close private file %q: %w", path, closeErr)
	}
	return contents, nil
}

// WritePrivateFile securely creates or replaces the contents of path. Callers
// that require exclusive creation and durability should use
// WriteNewPrivateFile.
func WritePrivateFile(path string, data []byte) error {
	file, err := openPrivateFile(path, os.O_WRONLY|os.O_CREATE, PrivateFileMode)
	if err != nil {
		return fmt.Errorf("open private file %q: %w", path, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if err := enforceFileMode(file, PrivateFileMode); err != nil {
		return fmt.Errorf("secure private file %q: %w", path, err)
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate private file %q: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write private file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private file %q: %w", path, err)
	}
	closed = true
	return nil
}

// WriteNewPrivateFile exclusively creates and durably writes path. A partial
// file is removed when writing or syncing fails before close.
func WriteNewPrivateFile(path string, data []byte) error {
	if path == "" {
		return errors.New("private file path is required")
	}
	file, err := openPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, PrivateFileMode)
	if err != nil {
		return fmt.Errorf("create private file %q: %w", path, err)
	}
	closed := false
	success := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if !success {
			_ = os.Remove(path)
		}
	}()
	if err := enforceFileMode(file, PrivateFileMode); err != nil {
		return fmt.Errorf("secure private file %q: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write private file %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync private file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private file %q: %w", path, err)
	}
	closed = true
	if err := SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync private file directory %q: %w", filepath.Dir(path), err)
	}
	success = true
	return nil
}

// SyncDir persists directory-entry changes on platforms that support
// directory fsync. Windows performs a safe existence/type check only.
func SyncDir(path string) error {
	if path == "" {
		return errors.New("directory path is required")
	}
	return syncDirectory(path)
}

func enforceDirectoryMode(directory *os.File, want fs.FileMode) error {
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("opened path is not a directory")
	}
	if err := directory.Chmod(want); err != nil {
		return err
	}
	info, err = directory.Stat()
	if err != nil {
		return err
	}
	return validateMode(info.Mode(), want)
}

func enforceFileMode(file *os.File, want fs.FileMode) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("opened path is not a regular file")
	}
	if err := file.Chmod(want); err != nil {
		return err
	}
	info, err = file.Stat()
	if err != nil {
		return err
	}
	return validateMode(info.Mode(), want)
}
