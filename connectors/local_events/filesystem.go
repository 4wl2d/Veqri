package local_events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type FileChange string

const (
	FileCreated  FileChange = "created"
	FileModified FileChange = "modified"
	FileRemoved  FileChange = "removed"
)

type FilesystemConfig struct {
	Root            string
	Paths           []string
	Interval        time.Duration
	EventType       string
	ConversationKey string
	CreateTask      bool
}

type FilesystemChange struct {
	Path       string     `json:"path"`
	Change     FileChange `json:"change"`
	SizeBytes  int64      `json:"size_bytes,omitempty"`
	Mode       uint32     `json:"mode,omitempty"`
	ModifiedAt time.Time  `json:"modified_at,omitempty"`
}

type fileSnapshot struct {
	exists     bool
	size       int64
	mode       os.FileMode
	modifiedAt time.Time
}

type FilesystemPoller struct {
	root            string
	paths           []string
	interval        time.Duration
	eventType       string
	conversationKey string
	createTask      bool
}

func NewFilesystemPoller(config FilesystemConfig) (*FilesystemPoller, error) {
	if strings.TrimSpace(config.Root) == "" {
		return nil, errors.New("filesystem poller root is required")
	}
	if len(config.Paths) == 0 {
		return nil, errors.New("filesystem poller requires at least one path")
	}
	if len(config.Paths) > 10_000 {
		return nil, errors.New("filesystem poller supports at most 10000 paths")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve filesystem poller root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve filesystem poller root symlinks: %w", err)
	}
	if info, statErr := os.Stat(root); statErr != nil || !info.IsDir() {
		return nil, errors.New("filesystem poller root is not a directory")
	}
	seen := make(map[string]bool)
	paths := make([]string, 0, len(config.Paths))
	for _, value := range config.Paths {
		if value == "" || filepath.IsAbs(value) || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("filesystem poller path %q must be a non-empty relative path", value)
		}
		clean := filepath.Clean(value)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("filesystem poller path %q escapes the root", value)
		}
		if !seen[clean] {
			seen[clean] = true
			paths = append(paths, clean)
		}
	}
	if config.Interval == 0 {
		config.Interval = 500 * time.Millisecond
	}
	if config.Interval < 10*time.Millisecond || config.Interval > time.Hour {
		return nil, errors.New("filesystem poll interval must be between 10 milliseconds and 1 hour")
	}
	if config.EventType == "" {
		config.EventType = "filesystem.changed"
	}
	if !validEventType(config.EventType) {
		return nil, errors.New("invalid filesystem event type")
	}
	return &FilesystemPoller{
		root: root, paths: paths, interval: config.Interval, eventType: config.EventType,
		conversationKey: config.ConversationKey, createTask: config.CreateTask,
	}, nil
}

// Run takes an initial snapshot without emitting it, then emits create,
// modification, and removal events until ctx is cancelled. Every poll rechecks
// symlink containment so a watched path cannot be swapped outside Root.
func (p *FilesystemPoller) Run(ctx context.Context, emit func(Event) error) error {
	if emit == nil {
		return errors.New("filesystem poller event callback is required")
	}
	snapshots := make(map[string]fileSnapshot, len(p.paths))
	for _, relative := range p.paths {
		snapshot, err := p.snapshot(relative)
		if err != nil {
			return err
		}
		snapshots[relative] = snapshot
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for _, relative := range p.paths {
				current, err := p.snapshot(relative)
				if err != nil {
					return err
				}
				previous := snapshots[relative]
				change := detectFileChange(previous, current)
				snapshots[relative] = current
				if change == "" {
					continue
				}
				payload := FilesystemChange{Path: filepath.ToSlash(relative), Change: change}
				if current.exists {
					payload.SizeBytes = current.size
					payload.Mode = uint32(current.mode.Perm())
					payload.ModifiedAt = current.modifiedAt.UTC()
				}
				data, err := json.Marshal(payload)
				if err != nil {
					return err
				}
				idempotencyKey, err := secureNonce()
				if err != nil {
					return fmt.Errorf("create filesystem event idempotency key: %w", err)
				}
				event := Event{
					Type: p.eventType, Data: data, ConversationKey: p.conversationKey,
					IdempotencyKey: idempotencyKey, CreateTask: p.createTask,
				}
				if err := emit(event); err != nil {
					return err
				}
			}
		}
	}
}

func (p *FilesystemPoller) snapshot(relative string) (fileSnapshot, error) {
	target := filepath.Join(p.root, relative)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		if !os.IsNotExist(err) {
			return fileSnapshot{}, fmt.Errorf("inspect watched path %q: %w", relative, err)
		}
		if err := p.verifyExistingParent(target); err != nil {
			return fileSnapshot{}, err
		}
		return fileSnapshot{}, nil
	}
	if !withinRoot(p.root, resolved) {
		return fileSnapshot{}, fmt.Errorf("watched path %q resolves outside the configured root", relative)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return fileSnapshot{}, nil
		}
		return fileSnapshot{}, fmt.Errorf("stat watched path %q: %w", relative, err)
	}
	return fileSnapshot{exists: true, size: info.Size(), mode: info.Mode(), modifiedAt: info.ModTime()}, nil
}

func (p *FilesystemPoller) verifyExistingParent(target string) error {
	current := filepath.Dir(target)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if !withinRoot(p.root, resolved) {
				return errors.New("watched path parent resolves outside the configured root")
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return errors.New("could not find an existing parent for watched path")
		}
		current = parent
	}
}

func withinRoot(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func detectFileChange(previous, current fileSnapshot) FileChange {
	switch {
	case !previous.exists && current.exists:
		return FileCreated
	case previous.exists && !current.exists:
		return FileRemoved
	case previous.exists && current.exists && (previous.size != current.size || previous.mode != current.mode || !previous.modifiedAt.Equal(current.modifiedAt)):
		return FileModified
	default:
		return ""
	}
}
