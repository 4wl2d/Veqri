// Package git exposes a constrained, structured Git tool. Callers choose a
// typed operation; arbitrary Git arguments and force pushes are never accepted.
package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	coretools "github.com/veqri/veqri/core/tools"
)

const (
	defaultTimeout        = 30 * time.Second
	defaultMaxOutputBytes = 1 << 20
	maxTimeout            = 10 * time.Minute
	maxCommitMessageBytes = 64 << 10
	maxPathspecs          = 1_000
	maxRefspecs           = 100
)

var (
	ErrOutsideWorkspace = errors.New("git path is outside an allowed workspace")
	ErrForcePush        = errors.New("force push is denied")
	ErrUnsafeRepository = errors.New("unsafe git repository configuration")
)

type Operation string

const (
	OperationStatus         Operation = "status"
	OperationDiff           Operation = "diff"
	OperationLog            Operation = "log"
	OperationBranchList     Operation = "branch_list"
	OperationBranchCreate   Operation = "branch_create"
	OperationBranchSwitch   Operation = "branch_switch"
	OperationBranchRename   Operation = "branch_rename"
	OperationBranchDelete   Operation = "branch_delete"
	OperationWorktreeCreate Operation = "worktree_create"
	OperationCommit         Operation = "commit"
	OperationPush           Operation = "push"
)

// Input intentionally has no raw argv or force field. Pathspecs are always
// placed after -- and revisions, branch names, remotes, and refspecs receive
// operation-specific validation.
type Input struct {
	Operation      Operation `json:"operation"`
	Repository     string    `json:"repository"`
	Paths          []string  `json:"paths,omitempty"`
	Staged         bool      `json:"staged,omitempty"`
	Base           string    `json:"base,omitempty"`
	Target         string    `json:"target,omitempty"`
	Revision       string    `json:"revision,omitempty"`
	Limit          int       `json:"limit,omitempty"`
	Branch         string    `json:"branch,omitempty"`
	NewBranch      string    `json:"new_branch,omitempty"`
	StartPoint     string    `json:"start_point,omitempty"`
	WorktreePath   string    `json:"worktree_path,omitempty"`
	CreateBranch   bool      `json:"create_branch,omitempty"`
	Detach         bool      `json:"detach,omitempty"`
	Message        string    `json:"message,omitempty"`
	All            bool      `json:"all,omitempty"`
	Remote         string    `json:"remote,omitempty"`
	Refspecs       []string  `json:"refspecs,omitempty"`
	SetUpstream    bool      `json:"set_upstream,omitempty"`
	TimeoutSeconds int       `json:"timeout_seconds,omitempty"`
	DryRun         bool      `json:"dry_run,omitempty"`
}

// Output is suitable for an audit record: it contains only the fixed binary,
// generated argv, canonical repository, risk decision, and bounded output.
type Output struct {
	Operation        Operation      `json:"operation"`
	Repository       string         `json:"repository"`
	Risk             coretools.Risk `json:"risk"`
	ApprovalRequired bool           `json:"approval_required"`
	Binary           string         `json:"binary"`
	Args             []string       `json:"args"`
	Stdout           string         `json:"stdout"`
	Stderr           string         `json:"stderr"`
	ExitCode         int            `json:"exit_code"`
	TimedOut         bool           `json:"timed_out"`
	Truncated        bool           `json:"truncated"`
	DryRun           bool           `json:"dry_run"`
}

type Config struct {
	Workspaces             []string
	GitBinary              string
	DefaultTimeout         time.Duration
	MaxOutputBytes         int
	UseUserConfig          bool
	AllowCredentialHelpers bool
	AllowedPushSchemes     []string
}

type Executor struct {
	workspaces             []string
	gitBinary              string
	sshBinary              string
	defaultTimeout         time.Duration
	maxOutputBytes         int
	useUserConfig          bool
	allowCredentialHelpers bool
	allowedPushSchemes     map[string]bool
}

var _ coretools.Executor = (*Executor)(nil)

func New(workspaces []string) (*Executor, error) {
	return NewWithConfig(Config{Workspaces: workspaces})
}

func NewWithConfig(config Config) (*Executor, error) {
	if len(config.Workspaces) == 0 {
		return nil, errors.New("at least one git workspace is required")
	}
	seen := make(map[string]bool)
	var workspaces []string
	for _, workspace := range config.Workspaces {
		absolute, err := filepath.Abs(workspace)
		if err != nil {
			return nil, fmt.Errorf("resolve git workspace %q: %w", workspace, err)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve git workspace %q: %w", workspace, err)
		}
		resolved = filepath.Clean(resolved)
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("git workspace %q is not a directory", workspace)
		}
		if !seen[resolved] {
			seen[resolved] = true
			workspaces = append(workspaces, resolved)
		}
	}
	sort.SliceStable(workspaces, func(i, j int) bool { return len(workspaces[i]) > len(workspaces[j]) })

	binary := config.GitBinary
	if binary == "" {
		var err error
		binary, err = exec.LookPath("git")
		if err != nil {
			return nil, errors.New("git executable was not found")
		}
	}
	absoluteBinary, err := filepath.Abs(binary)
	if err != nil {
		return nil, fmt.Errorf("resolve git executable: %w", err)
	}
	resolvedBinary, err := filepath.EvalSymlinks(absoluteBinary)
	if err != nil {
		return nil, fmt.Errorf("resolve git executable: %w", err)
	}
	if info, statErr := os.Stat(resolvedBinary); statErr != nil || !info.Mode().IsRegular() {
		return nil, errors.New("git executable is not a regular file")
	}

	if config.DefaultTimeout <= 0 {
		config.DefaultTimeout = defaultTimeout
	}
	if config.DefaultTimeout > maxTimeout {
		return nil, fmt.Errorf("default git timeout cannot exceed %s", maxTimeout)
	}
	if config.MaxOutputBytes <= 0 {
		config.MaxOutputBytes = defaultMaxOutputBytes
	}
	if len(config.AllowedPushSchemes) == 0 {
		config.AllowedPushSchemes = []string{"https", "ssh"}
	}
	allowedSchemes := make(map[string]bool, len(config.AllowedPushSchemes))
	for _, scheme := range config.AllowedPushSchemes {
		scheme = strings.ToLower(strings.TrimSpace(scheme))
		if !map[string]bool{"https": true, "ssh": true, "git": true, "file": true}[scheme] {
			return nil, fmt.Errorf("invalid push scheme %q", scheme)
		}
		allowedSchemes[scheme] = true
	}
	sshBinary, _ := exec.LookPath("ssh")
	if sshBinary != "" {
		sshBinary, _ = filepath.Abs(sshBinary)
		sshBinary, _ = filepath.EvalSymlinks(sshBinary)
	}
	return &Executor{
		workspaces: workspaces, gitBinary: resolvedBinary, sshBinary: sshBinary,
		defaultTimeout: config.DefaultTimeout, maxOutputBytes: config.MaxOutputBytes,
		useUserConfig: config.UseUserConfig, allowCredentialHelpers: config.AllowCredentialHelpers,
		allowedPushSchemes: allowedSchemes,
	}, nil
}

func (e *Executor) Definition() coretools.Definition {
	return coretools.Definition{
		Name:                 "git",
		Description:          "Runs a fixed set of structured Git operations inside allowed workspaces without force push",
		InputSchema:          json.RawMessage(`{"type":"object","additionalProperties":false,"required":["operation","repository"],"properties":{"operation":{"enum":["status","diff","log","branch_list","branch_create","branch_switch","branch_rename","branch_delete","worktree_create","commit","push"]},"repository":{"type":"string"},"paths":{"type":"array","items":{"type":"string"}},"staged":{"type":"boolean"},"base":{"type":"string"},"target":{"type":"string"},"revision":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":1000},"branch":{"type":"string"},"new_branch":{"type":"string"},"start_point":{"type":"string"},"worktree_path":{"type":"string"},"create_branch":{"type":"boolean"},"detach":{"type":"boolean"},"message":{"type":"string"},"all":{"type":"boolean"},"remote":{"type":"string"},"refspecs":{"type":"array","minItems":1,"maxItems":100,"items":{"type":"string"}},"set_upstream":{"type":"boolean"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600},"dry_run":{"type":"boolean"}}}`),
		OutputSchema:         json.RawMessage(`{"type":"object","required":["operation","repository","risk","approval_required","binary","args","stdout","stderr","exit_code"]}`),
		RequiredScopes:       []string{"tool.git"},
		Risk:                 coretools.RiskStateChanging,
		ApprovalRequired:     true,
		DefaultTimeout:       e.defaultTimeout,
		SupportsCancellation: true,
		SupportsStreaming:    true,
		SupportedOS:          []string{"darwin", "linux", "windows"},
	}
}

func Classify(input Input) coretools.Risk {
	switch input.Operation {
	case OperationStatus, OperationDiff, OperationLog, OperationBranchList:
		return coretools.RiskReadOnly
	case OperationBranchDelete:
		return coretools.RiskDestructive
	case OperationPush:
		return coretools.RiskExternalCommunication
	default:
		return coretools.RiskStateChanging
	}
}

func RequiresApproval(input Input) bool { return Classify(input) != coretools.RiskReadOnly }

func (e *Executor) ParseAndValidate(raw json.RawMessage) (Input, coretools.Risk, error) {
	var input Input
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return Input{}, "", fmt.Errorf("decode git input: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Input{}, "", errors.New("git input must contain exactly one JSON value")
		}
		return Input{}, "", fmt.Errorf("decode git input: %w", err)
	}
	if err := validateOperationFields(raw, input.Operation); err != nil {
		return Input{}, "", err
	}
	if len(input.Repository) > 4_096 {
		return Input{}, "", errors.New("repository cannot exceed 4096 bytes")
	}
	repository, err := e.resolveExisting(input.Repository)
	if err != nil {
		return Input{}, "", fmt.Errorf("validate repository: %w", err)
	}
	input.Repository = repository
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = int(e.defaultTimeout / time.Second)
		if input.TimeoutSeconds < 1 {
			input.TimeoutSeconds = 1
		}
	}
	if input.TimeoutSeconds < 1 || time.Duration(input.TimeoutSeconds)*time.Second > maxTimeout {
		return Input{}, "", errors.New("timeout_seconds must be between 1 and 600")
	}
	if len(input.Paths) > maxPathspecs {
		return Input{}, "", fmt.Errorf("paths cannot contain more than %d entries", maxPathspecs)
	}
	for _, path := range input.Paths {
		if err := validatePathspec(path); err != nil {
			return Input{}, "", err
		}
	}

	switch input.Operation {
	case OperationStatus:
		if err := rejectInputFields(input, "paths", len(input.Paths) != 0, "staged", input.Staged, "base", input.Base != "", "target", input.Target != "", "revision", input.Revision != "", "limit", input.Limit != 0, "branch", input.Branch != "", "new_branch", input.NewBranch != "", "start_point", input.StartPoint != "", "worktree_path", input.WorktreePath != "", "create_branch", input.CreateBranch, "detach", input.Detach, "message", input.Message != "", "all", input.All, "remote", input.Remote != "", "refspecs", len(input.Refspecs) != 0, "set_upstream", input.SetUpstream); err != nil {
			return Input{}, "", err
		}
	case OperationDiff:
		if err := validateRevision(input.Base); err != nil {
			return Input{}, "", fmt.Errorf("invalid base: %w", err)
		}
		if err := validateRevision(input.Target); err != nil {
			return Input{}, "", fmt.Errorf("invalid target: %w", err)
		}
		if input.Target != "" && input.Base == "" {
			return Input{}, "", errors.New("base is required when target is set")
		}
		if err := rejectInputFields(input, "revision", input.Revision != "", "limit", input.Limit != 0, "branch", input.Branch != "", "new_branch", input.NewBranch != "", "start_point", input.StartPoint != "", "worktree_path", input.WorktreePath != "", "create_branch", input.CreateBranch, "detach", input.Detach, "message", input.Message != "", "all", input.All, "remote", input.Remote != "", "refspecs", len(input.Refspecs) != 0, "set_upstream", input.SetUpstream); err != nil {
			return Input{}, "", err
		}
	case OperationLog:
		if err := validateRevision(input.Revision); err != nil {
			return Input{}, "", fmt.Errorf("invalid revision: %w", err)
		}
		if input.Limit == 0 {
			input.Limit = 50
		}
		if input.Limit < 1 || input.Limit > 1_000 {
			return Input{}, "", errors.New("limit must be between 1 and 1000")
		}
		if err := rejectInputFields(input, "staged", input.Staged, "base", input.Base != "", "target", input.Target != "", "branch", input.Branch != "", "new_branch", input.NewBranch != "", "start_point", input.StartPoint != "", "worktree_path", input.WorktreePath != "", "create_branch", input.CreateBranch, "detach", input.Detach, "message", input.Message != "", "all", input.All, "remote", input.Remote != "", "refspecs", len(input.Refspecs) != 0, "set_upstream", input.SetUpstream); err != nil {
			return Input{}, "", err
		}
	case OperationBranchList:
		if err := rejectAllOperationFields(input); err != nil {
			return Input{}, "", err
		}
	case OperationBranchCreate, OperationBranchSwitch, OperationBranchDelete:
		if err := validateBranch(input.Branch); err != nil {
			return Input{}, "", err
		}
		if input.Operation == OperationBranchCreate {
			if err := validateRevision(input.StartPoint); err != nil {
				return Input{}, "", fmt.Errorf("invalid start_point: %w", err)
			}
		} else if input.StartPoint != "" {
			return Input{}, "", errors.New("start_point is only valid for branch_create or worktree_create")
		}
		if err := rejectInputFields(input, "paths", len(input.Paths) != 0, "staged", input.Staged, "base", input.Base != "", "target", input.Target != "", "revision", input.Revision != "", "limit", input.Limit != 0, "new_branch", input.NewBranch != "", "worktree_path", input.WorktreePath != "", "create_branch", input.CreateBranch, "detach", input.Detach, "message", input.Message != "", "all", input.All, "remote", input.Remote != "", "refspecs", len(input.Refspecs) != 0, "set_upstream", input.SetUpstream); err != nil {
			return Input{}, "", err
		}
	case OperationBranchRename:
		if err := validateBranch(input.Branch); err != nil {
			return Input{}, "", err
		}
		if err := validateBranch(input.NewBranch); err != nil {
			return Input{}, "", fmt.Errorf("invalid new_branch: %w", err)
		}
		if err := rejectInputFields(input, "paths", len(input.Paths) != 0, "staged", input.Staged, "base", input.Base != "", "target", input.Target != "", "revision", input.Revision != "", "limit", input.Limit != 0, "start_point", input.StartPoint != "", "worktree_path", input.WorktreePath != "", "create_branch", input.CreateBranch, "detach", input.Detach, "message", input.Message != "", "all", input.All, "remote", input.Remote != "", "refspecs", len(input.Refspecs) != 0, "set_upstream", input.SetUpstream); err != nil {
			return Input{}, "", err
		}
	case OperationWorktreeCreate:
		if input.WorktreePath == "" {
			return Input{}, "", errors.New("worktree_path is required")
		}
		worktreePath, err := e.resolveMayNotExist(input.WorktreePath)
		if err != nil {
			return Input{}, "", fmt.Errorf("validate worktree_path: %w", err)
		}
		input.WorktreePath = worktreePath
		if input.Branch != "" {
			if err := validateBranch(input.Branch); err != nil {
				return Input{}, "", err
			}
		}
		if input.CreateBranch && input.Branch == "" {
			return Input{}, "", errors.New("branch is required when create_branch is true")
		}
		if input.CreateBranch && input.Detach {
			return Input{}, "", errors.New("create_branch and detach cannot both be true")
		}
		if err := validateRevision(input.StartPoint); err != nil {
			return Input{}, "", fmt.Errorf("invalid start_point: %w", err)
		}
		if err := rejectInputFields(input, "paths", len(input.Paths) != 0, "staged", input.Staged, "base", input.Base != "", "target", input.Target != "", "revision", input.Revision != "", "limit", input.Limit != 0, "new_branch", input.NewBranch != "", "message", input.Message != "", "all", input.All, "remote", input.Remote != "", "refspecs", len(input.Refspecs) != 0, "set_upstream", input.SetUpstream); err != nil {
			return Input{}, "", err
		}
	case OperationCommit:
		if input.Message == "" {
			return Input{}, "", errors.New("message is required for commit")
		}
		if len(input.Message) > maxCommitMessageBytes || strings.ContainsRune(input.Message, 0) {
			return Input{}, "", fmt.Errorf("commit message must be at most %d bytes and cannot contain NUL", maxCommitMessageBytes)
		}
		if input.All && len(input.Paths) != 0 {
			return Input{}, "", errors.New("all and paths cannot both be set")
		}
		if err := rejectInputFields(input, "staged", input.Staged, "base", input.Base != "", "target", input.Target != "", "revision", input.Revision != "", "limit", input.Limit != 0, "branch", input.Branch != "", "new_branch", input.NewBranch != "", "start_point", input.StartPoint != "", "worktree_path", input.WorktreePath != "", "create_branch", input.CreateBranch, "detach", input.Detach, "remote", input.Remote != "", "refspecs", len(input.Refspecs) != 0, "set_upstream", input.SetUpstream); err != nil {
			return Input{}, "", err
		}
	case OperationPush:
		if !remoteNamePattern.MatchString(input.Remote) {
			return Input{}, "", errors.New("remote must be a configured remote name, not a URL")
		}
		if len(input.Refspecs) == 0 || len(input.Refspecs) > maxRefspecs {
			return Input{}, "", fmt.Errorf("push requires between 1 and %d explicit refspecs", maxRefspecs)
		}
		for _, refspec := range input.Refspecs {
			if err := validateRefspec(refspec); err != nil {
				return Input{}, "", err
			}
		}
		if err := rejectInputFields(input, "paths", len(input.Paths) != 0, "staged", input.Staged, "base", input.Base != "", "target", input.Target != "", "revision", input.Revision != "", "limit", input.Limit != 0, "branch", input.Branch != "", "new_branch", input.NewBranch != "", "start_point", input.StartPoint != "", "worktree_path", input.WorktreePath != "", "create_branch", input.CreateBranch, "detach", input.Detach, "message", input.Message != "", "all", input.All); err != nil {
			return Input{}, "", err
		}
	default:
		return Input{}, "", fmt.Errorf("unsupported git operation %q", input.Operation)
	}
	return input, Classify(input), nil
}

func validateOperationFields(raw json.RawMessage, operation Operation) error {
	common := []string{"operation", "repository", "timeout_seconds", "dry_run"}
	var specific []string
	switch operation {
	case OperationStatus, OperationBranchList:
	case OperationDiff:
		specific = []string{"paths", "staged", "base", "target"}
	case OperationLog:
		specific = []string{"paths", "revision", "limit"}
	case OperationBranchCreate:
		specific = []string{"branch", "start_point"}
	case OperationBranchSwitch, OperationBranchDelete:
		specific = []string{"branch"}
	case OperationBranchRename:
		specific = []string{"branch", "new_branch"}
	case OperationWorktreeCreate:
		specific = []string{"branch", "start_point", "worktree_path", "create_branch", "detach"}
	case OperationCommit:
		specific = []string{"paths", "message", "all"}
	case OperationPush:
		specific = []string{"remote", "refspecs", "set_upstream"}
	default:
		return fmt.Errorf("unsupported git operation %q", operation)
	}
	allowed := make(map[string]bool, len(common)+len(specific))
	for _, name := range append(common, specific...) {
		allowed[name] = true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("decode git fields: %w", err)
	}
	for name := range fields {
		if !allowed[name] {
			return fmt.Errorf("field %s is not valid for %s", name, operation)
		}
	}
	return nil
}

func rejectAllOperationFields(input Input) error {
	return rejectInputFields(input, "paths", len(input.Paths) != 0, "staged", input.Staged, "base", input.Base != "", "target", input.Target != "", "revision", input.Revision != "", "limit", input.Limit != 0, "branch", input.Branch != "", "new_branch", input.NewBranch != "", "start_point", input.StartPoint != "", "worktree_path", input.WorktreePath != "", "create_branch", input.CreateBranch, "detach", input.Detach, "message", input.Message != "", "all", input.All, "remote", input.Remote != "", "refspecs", len(input.Refspecs) != 0, "set_upstream", input.SetUpstream)
}

func rejectInputFields(_ Input, fields ...any) error {
	for index := 0; index < len(fields); index += 2 {
		if fields[index+1].(bool) {
			return fmt.Errorf("field %s is not valid for this operation", fields[index].(string))
		}
	}
	return nil
}

var remoteNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validatePathspec(value string) error {
	if value == "" || strings.ContainsRune(value, 0) || filepath.IsAbs(value) || !filepath.IsLocal(value) {
		return fmt.Errorf("pathspec %q must be a local repository-relative path", value)
	}
	if strings.HasPrefix(value, ":") {
		return fmt.Errorf("pathspec magic is denied in %q", value)
	}
	return nil
}

func validateRevision(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 1_024 || strings.HasPrefix(value, "-") || strings.ContainsRune(value, 0) {
		return errors.New("revision is invalid")
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return errors.New("revision cannot contain whitespace or control characters")
		}
	}
	return nil
}

func validateBranch(value string) error {
	if value == "" || value == "@" || len(value) > 255 || strings.HasPrefix(value, "-") || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.HasSuffix(value, "/") || strings.HasSuffix(strings.ToLower(value), ".lock") || strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.ContainsAny(value, " ~^:?*[\\\x00") {
		return fmt.Errorf("invalid branch name %q", value)
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") || strings.HasSuffix(strings.ToLower(component), ".lock") {
			return fmt.Errorf("invalid branch name %q", value)
		}
	}
	return nil
}

func validateRefspec(value string) error {
	if value == "" || len(value) > 1_024 || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") || strings.Contains(value, "*") || strings.ContainsRune(value, 0) {
		if strings.HasPrefix(value, "+") {
			return ErrForcePush
		}
		return fmt.Errorf("invalid push refspec %q", value)
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf("invalid push refspec %q", value)
		}
	}
	parts := strings.Split(value, ":")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
		return fmt.Errorf("deleting refs and empty refspec components are denied in %q", value)
	}
	for _, part := range parts {
		if err := validateRevision(part); err != nil {
			return fmt.Errorf("invalid push refspec %q", value)
		}
	}
	return nil
}

func (e *Executor) Execute(ctx context.Context, raw json.RawMessage, progress func(coretools.Progress)) (json.RawMessage, error) {
	input, risk, err := e.ParseAndValidate(raw)
	if err != nil {
		return nil, err
	}
	commandContext, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	repository, gitDirectory, commonDirectory, err := e.inspectRepository(commandContext, input.Repository)
	if err != nil {
		return nil, err
	}
	input.Repository = repository
	if input.Operation == OperationBranchSwitch || input.Operation == OperationWorktreeCreate || (input.Operation == OperationCommit && (input.All || len(input.Paths) != 0)) {
		if err := e.ensureCheckoutSafe(gitDirectory, commonDirectory); err != nil {
			return nil, err
		}
	}
	if input.Operation == OperationPush {
		remoteURL, err := e.remotePushURL(commandContext, repository, input.Remote)
		if err != nil {
			return nil, err
		}
		if err := e.validateRemoteURL(repository, remoteURL); err != nil {
			return nil, err
		}
	}
	args := e.buildArgs(input)
	output := Output{Operation: input.Operation, Repository: repository, Risk: risk, ApprovalRequired: RequiresApproval(input), Binary: e.gitBinary, Args: append([]string(nil), args...), ExitCode: 0, DryRun: input.DryRun}
	if input.DryRun {
		return json.Marshal(output)
	}

	stdout := newLimitedBuffer(e.maxOutputBytes)
	stderr := newLimitedBuffer(e.maxOutputBytes)
	command := exec.CommandContext(commandContext, e.gitBinary, args...)
	command.Dir = repository
	command.Env = e.environment(input.Operation)
	command.Stdout = io.MultiWriter(stdout, &progressWriter{stream: "stdout", emit: progress, remaining: e.maxOutputBytes})
	command.Stderr = io.MultiWriter(stderr, &progressWriter{stream: "stderr", emit: progress, remaining: e.maxOutputBytes})
	runErr := command.Run()
	if runErr != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.As(runErr, &exitErr):
			output.ExitCode = exitErr.ExitCode()
		case commandContext.Err() != nil:
			output.ExitCode = -1
		default:
			return nil, fmt.Errorf("execute git: %w", runErr)
		}
	}
	output.Stdout, output.Stderr = stdout.String(), stderr.String()
	output.TimedOut = errors.Is(commandContext.Err(), context.DeadlineExceeded)
	output.Truncated = stdout.Truncated() || stderr.Truncated()
	encoded, encodeErr := json.Marshal(output)
	if encodeErr != nil {
		return nil, encodeErr
	}
	if runErr != nil {
		return encoded, fmt.Errorf("git %s exited with code %d", input.Operation, output.ExitCode)
	}
	return encoded, nil
}

func (e *Executor) buildArgs(input Input) []string {
	args := e.globalArgs(input.Repository)
	switch input.Operation {
	case OperationStatus:
		args = append(args, "status", "--porcelain=v2", "--branch", "--untracked-files=all")
	case OperationDiff:
		args = append(args, "diff", "--no-ext-diff", "--no-textconv", "--no-color")
		if input.Staged {
			args = append(args, "--cached")
		}
		if input.Base != "" {
			args = append(args, input.Base)
		}
		if input.Target != "" {
			args = append(args, input.Target)
		}
		args = append(args, "--")
		args = append(args, input.Paths...)
	case OperationLog:
		args = append(args, "log", "--no-show-signature", "--no-color", "--decorate=no", "--date=iso-strict", "--format=%H%x09%P%x09%an%x09%ae%x09%aI%x09%s", fmt.Sprintf("--max-count=%d", input.Limit))
		if input.Revision != "" {
			args = append(args, input.Revision)
		}
		args = append(args, "--")
		args = append(args, input.Paths...)
	case OperationBranchList:
		args = append(args, "branch", "--no-color", "--format=%(refname:short)%09%(objectname)%09%(HEAD)")
	case OperationBranchCreate:
		args = append(args, "branch", input.Branch)
		if input.StartPoint != "" {
			args = append(args, input.StartPoint)
		}
	case OperationBranchSwitch:
		args = append(args, "switch", "--no-guess", input.Branch)
	case OperationBranchRename:
		args = append(args, "branch", "--move", input.Branch, input.NewBranch)
	case OperationBranchDelete:
		args = append(args, "branch", "--delete", input.Branch)
	case OperationWorktreeCreate:
		args = append(args, "worktree", "add")
		if input.CreateBranch {
			args = append(args, "-b", input.Branch)
		} else if input.Detach {
			args = append(args, "--detach")
		}
		args = append(args, "--", input.WorktreePath)
		if input.StartPoint != "" {
			args = append(args, input.StartPoint)
		} else if input.Branch != "" && !input.CreateBranch {
			args = append(args, input.Branch)
		}
	case OperationCommit:
		args = append(args, "commit", "--no-verify", "--no-gpg-sign", "--cleanup=verbatim", "-m", input.Message)
		if input.All {
			args = append(args, "--all")
		}
		if len(input.Paths) != 0 {
			args = append(args, "--only", "--")
			args = append(args, input.Paths...)
		}
	case OperationPush:
		args = append(args, "push", "--porcelain", "--no-verify", "--no-force", "--no-signed")
		if input.SetUpstream {
			args = append(args, "--set-upstream")
		}
		args = append(args, "--", input.Remote)
		args = append(args, input.Refspecs...)
	}
	return args
}

func (e *Executor) globalArgs(repository string) []string {
	args := []string{
		"--no-pager",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "diff.external=",
		"-c", "protocol.ext.allow=never",
		"-c", "commit.gpgSign=false",
		"-c", "push.gpgSign=false",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
	}
	if !e.allowCredentialHelpers {
		args = append(args, "-c", "credential.helper=")
	}
	if e.sshBinary != "" {
		args = append(args, "-c", "core.sshCommand="+e.sshBinary)
	}
	return append(args, "-C", repository)
}

func (e *Executor) environment(operation Operation) []string {
	values := map[string]string{
		"LANG": "C", "LC_ALL": "C", "GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0",
	}
	if operation != OperationStatus && operation != OperationDiff && operation != OperationLog && operation != OperationBranchList {
		values["GIT_OPTIONAL_LOCKS"] = "1"
	}
	if path := os.Getenv("PATH"); path != "" {
		values["PATH"] = path
	}
	if e.useUserConfig {
		for _, name := range []string{"HOME", "XDG_CONFIG_HOME"} {
			if value := os.Getenv(name); value != "" {
				values[name] = value
			}
		}
	} else {
		values["GIT_CONFIG_NOSYSTEM"] = "1"
		values["GIT_CONFIG_GLOBAL"] = os.DevNull
	}
	if operation == OperationPush {
		if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
			values["SSH_AUTH_SOCK"] = socket
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func (e *Executor) inspectRepository(ctx context.Context, repository string) (string, string, string, error) {
	args := append(e.globalArgs(repository), "rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	stdout, stderr, exitCode, err := e.runInternal(ctx, repository, args)
	if err != nil {
		return "", "", "", err
	}
	if exitCode != 0 {
		return "", "", "", fmt.Errorf("inspect git repository: %s", strings.TrimSpace(stderr))
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		return "", "", "", errors.New("git returned an unexpected repository description")
	}
	paths := make([]string, 3)
	for index, value := range lines {
		resolved, err := e.resolveExisting(strings.TrimSpace(value))
		if err != nil {
			return "", "", "", fmt.Errorf("%w: git metadata path %q: %v", ErrUnsafeRepository, value, err)
		}
		paths[index] = resolved
	}
	return paths[0], paths[1], paths[2], nil
}

func (e *Executor) runInternal(ctx context.Context, repository string, args []string) (string, string, int, error) {
	stdout := newLimitedBuffer(64 << 10)
	stderr := newLimitedBuffer(64 << 10)
	command := exec.CommandContext(ctx, e.gitBinary, args...)
	command.Dir = repository
	command.Env = e.environment(OperationStatus)
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), stderr.String(), exitErr.ExitCode(), nil
	}
	if ctx.Err() != nil {
		return "", "", -1, ctx.Err()
	}
	return "", "", -1, err
}

func (e *Executor) remotePushURL(ctx context.Context, repository, remote string) (string, error) {
	args := append(e.globalArgs(repository), "remote", "get-url", "--push", "--", remote)
	stdout, stderr, exitCode, err := e.runInternal(ctx, repository, args)
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		return "", fmt.Errorf("resolve push remote %q: %s", remote, strings.TrimSpace(stderr))
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 || lines[0] == "" {
		return "", errors.New("push remote must resolve to exactly one URL")
	}
	return lines[0], nil
}

func (e *Executor) validateRemoteURL(repository, value string) error {
	if strings.Contains(value, "::") {
		return errors.New("Git remote helpers are denied")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("parse push URL: %w", err)
	}
	if parsed.Scheme != "" {
		scheme := strings.ToLower(parsed.Scheme)
		if !e.allowedPushSchemes[scheme] {
			return fmt.Errorf("push URL scheme %q is not allowed", scheme)
		}
		if scheme == "file" {
			if parsed.Host != "" && parsed.Host != "localhost" {
				return errors.New("file push URL cannot name a remote host")
			}
			_, err := e.resolveExisting(parsed.Path)
			return err
		}
		if parsed.User != nil {
			if _, hasPassword := parsed.User.Password(); hasPassword {
				return errors.New("plaintext credentials in a push URL are denied")
			}
			if !safeUserPattern.MatchString(parsed.User.Username()) {
				return errors.New("push URL contains an unsafe username")
			}
		}
		if !validNetworkHost(parsed.Hostname()) {
			return errors.New("network push URL requires a host")
		}
		if scheme == "ssh" && e.sshBinary == "" {
			return errors.New("ssh push is unavailable because no ssh executable was found")
		}
		return nil
	}
	// SCP-style SSH syntax is common and has no URL scheme.
	if colon := strings.IndexByte(value, ':'); colon > 0 && strings.Contains(value[:colon], "@") {
		if !e.allowedPushSchemes["ssh"] || e.sshBinary == "" {
			return errors.New("ssh push URLs are not allowed")
		}
		userHost := value[:colon]
		user, host, found := strings.Cut(userHost, "@")
		if !found || !safeUserPattern.MatchString(user) || !validNetworkHost(host) || value[colon+1:] == "" {
			return errors.New("invalid SCP-style push URL")
		}
		return nil
	}
	// A local path is allowed only when the file scheme is explicitly enabled
	// and both repositories remain in configured workspaces.
	if !e.allowedPushSchemes["file"] {
		return errors.New("local filesystem push URLs are not allowed")
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(repository, path)
	}
	_, err = e.resolveExisting(path)
	return err
}

func (e *Executor) ensureCheckoutSafe(gitDirectory, commonDirectory string) error {
	paths := []string{filepath.Join(commonDirectory, "config")}
	if gitDirectory != commonDirectory {
		paths = append(paths, filepath.Join(gitDirectory, "config.worktree"))
	}
	sectionPattern := regexp.MustCompile(`(?im)^\s*\[(include(?:if)?|filter\s+[^]]+)\s*\]`)
	commandPattern := regexp.MustCompile(`(?im)^\s*(clean|smudge|process)\s*=`)
	for _, path := range paths {
		content, exists, err := e.safeReadSmall(path, 1<<20)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if sectionPattern.Match(content) || commandPattern.Match(content) {
			return fmt.Errorf("%w: checkout-capable operations deny config includes and external content filters", ErrUnsafeRepository)
		}
	}
	return nil
}

func (e *Executor) safeReadSmall(path string, limit int64) ([]byte, bool, error) {
	resolvedPath, err := e.resolveExisting(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		if _, statErr := os.Lstat(path); errors.Is(statErr, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, false, fmt.Errorf("%w: unsafe or oversized config %q", ErrUnsafeRepository, path)
	}
	file, err := os.Open(resolvedPath)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(content)) > limit {
		return nil, false, fmt.Errorf("%w: oversized config %q", ErrUnsafeRepository, path)
	}
	return content, true, nil
}

func (e *Executor) resolveExisting(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, 0) {
		return "", errors.New("path is required and cannot contain NUL")
	}
	path := value
	if !filepath.IsAbs(path) {
		if !filepath.IsLocal(path) {
			return "", ErrOutsideWorkspace
		}
		path = filepath.Join(e.workspaces[0], path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	resolved = filepath.Clean(resolved)
	if !e.contains(resolved) {
		return "", ErrOutsideWorkspace
	}
	return resolved, nil
}

func (e *Executor) resolveMayNotExist(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, 0) {
		return "", errors.New("path is required and cannot contain NUL")
	}
	path := value
	if !filepath.IsAbs(path) {
		if !filepath.IsLocal(path) {
			return "", ErrOutsideWorkspace
		}
		path = filepath.Join(e.workspaces[0], path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	ancestor := absolute
	for {
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if ancestor == absolute && info.Mode()&fs.ModeSymlink != 0 {
				return "", errors.New("worktree path cannot be a symlink")
			}
			break
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", ErrOutsideWorkspace
		}
		ancestor = parent
	}
	resolvedAncestor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	if !e.contains(filepath.Clean(resolvedAncestor)) {
		return "", ErrOutsideWorkspace
	}
	suffix, err := filepath.Rel(ancestor, absolute)
	if err != nil || (!filepath.IsLocal(suffix) && suffix != ".") {
		return "", ErrOutsideWorkspace
	}
	canonical := filepath.Clean(filepath.Join(resolvedAncestor, suffix))
	if !e.contains(canonical) {
		return "", ErrOutsideWorkspace
	}
	if e.isWorkspaceRoot(canonical) {
		return "", errors.New("worktree path cannot be a workspace root")
	}
	return canonical, nil
}

func (e *Executor) contains(path string) bool {
	for _, workspace := range e.workspaces {
		relative, err := filepath.Rel(workspace, path)
		if err == nil && (relative == "." || filepath.IsLocal(relative)) {
			return true
		}
	}
	return false
}

func (e *Executor) isWorkspaceRoot(path string) bool {
	for _, workspace := range e.workspaces {
		if path == workspace {
			return true
		}
	}
	return false
}

type limitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer { return &limitedBuffer{limit: limit} }

func (b *limitedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLength := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return originalLength, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(value)
	return originalLength, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

type progressWriter struct {
	mu        sync.Mutex
	stream    string
	emit      func(coretools.Progress)
	remaining int
}

func (w *progressWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	originalLength := len(value)
	if len(value) > w.remaining {
		value = value[:w.remaining]
	}
	if w.emit != nil && len(value) != 0 {
		w.emit(coretools.Progress{Stream: w.stream, Data: string(value)})
	}
	w.remaining -= len(value)
	return originalLength, nil
}

var safeUserPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validNetworkHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if host == "" || len(host) > 253 || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') && !(character >= '0' && character <= '9') && character != '-' {
				return false
			}
		}
	}
	return true
}
