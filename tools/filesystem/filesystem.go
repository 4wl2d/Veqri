// Package filesystem implements a workspace-confined, typed filesystem tool.
//
// All file access is performed through os.Root. Unlike a path-prefix check,
// os.Root keeps each operation beneath an opened workspace even when an
// attacker races symlinks while the operation is running.
package filesystem

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	coretools "github.com/veqri/veqri/core/tools"
)

const (
	defaultMaxReadBytes       int64 = 1 << 20
	defaultMaxWriteBytes      int64 = 1 << 20
	defaultMaxSearchFileBytes int64 = 1 << 20
	defaultMaxHashBytes       int64 = 64 << 20
	defaultMaxSearchResults         = 1_000
	defaultMaxSearchFiles           = 10_000
	defaultMaxListEntries           = 10_000
	maxPatchEdits                   = 100
)

var (
	ErrOutsideWorkspace = errors.New("path is outside an allowed workspace")
	ErrTooLarge         = errors.New("filesystem size limit exceeded")
	ErrConflict         = errors.New("filesystem precondition failed")
	ErrUnsupportedType  = errors.New("unsupported filesystem object type")
)

type Operation string

const (
	OperationRead   Operation = "read"
	OperationList   Operation = "list"
	OperationSearch Operation = "search"
	OperationWrite  Operation = "write"
	OperationPatch  Operation = "patch"
	OperationMove   Operation = "move"
	OperationDelete Operation = "delete"
)

type Encoding string

const (
	EncodingUTF8   Encoding = "utf8"
	EncodingBase64 Encoding = "base64"
)

type SearchMode string

const (
	SearchContents SearchMode = "contents"
	SearchNames    SearchMode = "names"
)

// ExactPatch replaces OldText only when it occurs exactly ExpectedOccurrences
// times. ExpectedOccurrences defaults to one. No write occurs unless every
// edit in a request satisfies its precondition.
type ExactPatch struct {
	OldText             string `json:"old_text"`
	NewText             string `json:"new_text"`
	ExpectedOccurrences int    `json:"expected_occurrences,omitempty"`
}

// Input is a tagged request. Fields not used by the selected operation are
// rejected so an approval record describes exactly what will be executed.
type Input struct {
	Operation      Operation    `json:"operation"`
	Path           string       `json:"path"`
	Destination    string       `json:"destination,omitempty"`
	Content        string       `json:"content,omitempty"`
	Encoding       Encoding     `json:"encoding,omitempty"`
	ExpectedSHA256 string       `json:"expected_sha256,omitempty"`
	MustNotExist   bool         `json:"must_not_exist,omitempty"`
	Patches        []ExactPatch `json:"patches,omitempty"`
	Query          string       `json:"query,omitempty"`
	Regex          bool         `json:"regex,omitempty"`
	CaseSensitive  bool         `json:"case_sensitive,omitempty"`
	SearchMode     SearchMode   `json:"search_mode,omitempty"`
	Recursive      bool         `json:"recursive,omitempty"`
	MaxBytes       int64        `json:"max_bytes,omitempty"`
	MaxResults     int          `json:"max_results,omitempty"`
	CreateParents  bool         `json:"create_parents,omitempty"`
	Overwrite      bool         `json:"overwrite,omitempty"`
	DryRun         bool         `json:"dry_run,omitempty"`
}

type Entry struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Size       int64     `json:"size,omitempty"`
	Mode       string    `json:"mode"`
	ModifiedAt time.Time `json:"modified_at"`
}

type Match struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Snippet string `json:"snippet"`
}

// Output includes the operation-level risk decision so it can be persisted in
// an audit record alongside the normalized paths and content hashes.
type Output struct {
	Operation        Operation      `json:"operation"`
	Workspace        string         `json:"workspace"`
	Path             string         `json:"path"`
	Destination      string         `json:"destination,omitempty"`
	Risk             coretools.Risk `json:"risk"`
	ApprovalRequired bool           `json:"approval_required"`
	Content          string         `json:"content,omitempty"`
	Encoding         Encoding       `json:"encoding,omitempty"`
	Entries          []Entry        `json:"entries,omitempty"`
	Matches          []Match        `json:"matches,omitempty"`
	SHA256           string         `json:"sha256,omitempty"`
	PreviousSHA256   string         `json:"previous_sha256,omitempty"`
	Bytes            int64          `json:"bytes,omitempty"`
	FilesScanned     int            `json:"files_scanned,omitempty"`
	FilesSkipped     int            `json:"files_skipped,omitempty"`
	Truncated        bool           `json:"truncated,omitempty"`
	DryRun           bool           `json:"dry_run,omitempty"`
}

type Config struct {
	Workspaces         []string
	MaxReadBytes       int64
	MaxWriteBytes      int64
	MaxSearchFileBytes int64
	MaxHashBytes       int64
	MaxSearchResults   int
	MaxSearchFiles     int
	MaxListEntries     int
	WriteMode          fs.FileMode
}

type workspace struct {
	path    string
	aliases []string
	root    *os.Root
}

type Executor struct {
	workspaces         []workspace
	maxReadBytes       int64
	maxWriteBytes      int64
	maxSearchFileBytes int64
	maxHashBytes       int64
	maxSearchResults   int
	maxSearchFiles     int
	maxListEntries     int
	writeMode          fs.FileMode
}

var _ coretools.Executor = (*Executor)(nil)

func New(workspaces []string) (*Executor, error) {
	return NewWithConfig(Config{Workspaces: workspaces})
}

func NewWithConfig(config Config) (*Executor, error) {
	if len(config.Workspaces) == 0 {
		return nil, errors.New("at least one filesystem workspace is required")
	}
	if config.MaxReadBytes <= 0 {
		config.MaxReadBytes = defaultMaxReadBytes
	}
	if config.MaxWriteBytes <= 0 {
		config.MaxWriteBytes = defaultMaxWriteBytes
	}
	if config.MaxSearchFileBytes <= 0 {
		config.MaxSearchFileBytes = defaultMaxSearchFileBytes
	}
	if config.MaxHashBytes <= 0 {
		config.MaxHashBytes = defaultMaxHashBytes
	}
	if config.MaxSearchResults <= 0 {
		config.MaxSearchResults = defaultMaxSearchResults
	}
	if config.MaxSearchFiles <= 0 {
		config.MaxSearchFiles = defaultMaxSearchFiles
	}
	if config.MaxListEntries <= 0 {
		config.MaxListEntries = defaultMaxListEntries
	}
	if config.WriteMode == 0 {
		config.WriteMode = 0o600
	}
	if config.WriteMode&^0o777 != 0 {
		return nil, errors.New("write mode may only contain permission bits")
	}

	result := &Executor{
		maxReadBytes: config.MaxReadBytes, maxWriteBytes: config.MaxWriteBytes,
		maxSearchFileBytes: config.MaxSearchFileBytes, maxHashBytes: config.MaxHashBytes,
		maxSearchResults: config.MaxSearchResults, maxSearchFiles: config.MaxSearchFiles,
		maxListEntries: config.MaxListEntries, writeMode: config.WriteMode,
	}
	seen := make(map[string]int)
	for _, value := range config.Workspaces {
		absolute, err := filepath.Abs(value)
		if err != nil {
			result.Close()
			return nil, fmt.Errorf("resolve workspace %q: %w", value, err)
		}
		absolute = filepath.Clean(absolute)
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			result.Close()
			return nil, fmt.Errorf("resolve workspace %q: %w", value, err)
		}
		resolved = filepath.Clean(resolved)
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			result.Close()
			return nil, fmt.Errorf("workspace %q is not a directory", value)
		}
		if index, ok := seen[resolved]; ok {
			if !containsString(result.workspaces[index].aliases, absolute) {
				result.workspaces[index].aliases = append(result.workspaces[index].aliases, absolute)
			}
			continue
		}
		root, err := os.OpenRoot(resolved)
		if err != nil {
			result.Close()
			return nil, fmt.Errorf("open workspace %q: %w", value, err)
		}
		aliases := []string{resolved}
		if absolute != resolved {
			aliases = append(aliases, absolute)
		}
		seen[resolved] = len(result.workspaces)
		result.workspaces = append(result.workspaces, workspace{path: resolved, aliases: aliases, root: root})
	}
	// Prefer the most specific root for absolute paths when workspaces overlap.
	sort.SliceStable(result.workspaces, func(i, j int) bool {
		return len(result.workspaces[i].path) > len(result.workspaces[j].path)
	})
	return result, nil
}

func (e *Executor) Close() error {
	var errs []error
	for index := range e.workspaces {
		if e.workspaces[index].root != nil {
			errs = append(errs, e.workspaces[index].root.Close())
			e.workspaces[index].root = nil
		}
	}
	return errors.Join(errs...)
}

func (e *Executor) Definition() coretools.Definition {
	return coretools.Definition{
		Name:                 "filesystem",
		Description:          "Reads and changes files through traversal-resistant allowed-workspace roots",
		InputSchema:          json.RawMessage(`{"type":"object","additionalProperties":false,"required":["operation","path"],"properties":{"operation":{"enum":["read","list","search","write","patch","move","delete"]},"path":{"type":"string"},"destination":{"type":"string"},"content":{"type":"string"},"encoding":{"enum":["utf8","base64"]},"expected_sha256":{"type":"string","pattern":"^[a-fA-F0-9]{64}$"},"must_not_exist":{"type":"boolean"},"patches":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["old_text","new_text"],"properties":{"old_text":{"type":"string"},"new_text":{"type":"string"},"expected_occurrences":{"type":"integer","minimum":1}}}},"query":{"type":"string"},"regex":{"type":"boolean"},"case_sensitive":{"type":"boolean"},"search_mode":{"enum":["contents","names"]},"recursive":{"type":"boolean"},"max_bytes":{"type":"integer","minimum":1},"max_results":{"type":"integer","minimum":1},"create_parents":{"type":"boolean"},"overwrite":{"type":"boolean"},"dry_run":{"type":"boolean"}}}`),
		OutputSchema:         json.RawMessage(`{"type":"object","required":["operation","workspace","path","risk","approval_required"]}`),
		RequiredScopes:       []string{"tool.filesystem"},
		Risk:                 coretools.RiskStateChanging,
		ApprovalRequired:     true,
		DefaultTimeout:       30 * time.Second,
		SupportsCancellation: true,
		SupportsStreaming:    false,
		SupportedOS:          []string{"darwin", "linux", "windows"},
	}
}

func Classify(input Input) coretools.Risk {
	switch input.Operation {
	case OperationRead, OperationList, OperationSearch:
		return coretools.RiskReadOnly
	case OperationDelete:
		return coretools.RiskDestructive
	case OperationMove:
		if input.Overwrite {
			return coretools.RiskDestructive
		}
		return coretools.RiskStateChanging
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
		return Input{}, "", fmt.Errorf("decode filesystem input: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Input{}, "", err
	}
	if err := validateOperationFields(raw, input.Operation); err != nil {
		return Input{}, "", err
	}
	if len(input.Path) > 4_096 {
		return Input{}, "", errors.New("path cannot exceed 4096 bytes")
	}
	if _, _, _, err := e.resolve(input.Path); err != nil {
		return Input{}, "", fmt.Errorf("validate path: %w", err)
	}
	if input.ExpectedSHA256 != "" {
		if len(input.ExpectedSHA256) != sha256.Size*2 {
			return Input{}, "", errors.New("expected_sha256 must contain 64 hexadecimal characters")
		}
		if _, err := hex.DecodeString(input.ExpectedSHA256); err != nil {
			return Input{}, "", errors.New("expected_sha256 must contain 64 hexadecimal characters")
		}
		input.ExpectedSHA256 = strings.ToLower(input.ExpectedSHA256)
	}
	if input.Encoding == "" {
		input.Encoding = EncodingUTF8
	}
	if input.Encoding != EncodingUTF8 && input.Encoding != EncodingBase64 {
		return Input{}, "", errors.New("encoding must be utf8 or base64")
	}

	switch input.Operation {
	case OperationRead:
		if err := rejectFields(input, "destination", input.Destination != "", "content", input.Content != "", "patches", len(input.Patches) != 0, "query", input.Query != "", "recursive", input.Recursive, "create_parents", input.CreateParents, "overwrite", input.Overwrite, "must_not_exist", input.MustNotExist, "dry_run", input.DryRun); err != nil {
			return Input{}, "", err
		}
		if input.MaxBytes == 0 {
			input.MaxBytes = e.maxReadBytes
		}
		if input.MaxBytes < 1 || input.MaxBytes > e.maxReadBytes {
			return Input{}, "", fmt.Errorf("max_bytes must be between 1 and %d", e.maxReadBytes)
		}
	case OperationList:
		if err := rejectFields(input, "destination", input.Destination != "", "content", input.Content != "", "patches", len(input.Patches) != 0, "query", input.Query != "", "max_bytes", input.MaxBytes != 0, "create_parents", input.CreateParents, "overwrite", input.Overwrite, "must_not_exist", input.MustNotExist, "dry_run", input.DryRun); err != nil {
			return Input{}, "", err
		}
		if input.MaxResults == 0 {
			input.MaxResults = e.maxListEntries
		}
		if input.MaxResults < 1 || input.MaxResults > e.maxListEntries {
			return Input{}, "", fmt.Errorf("max_results must be between 1 and %d", e.maxListEntries)
		}
	case OperationSearch:
		if input.Query == "" {
			return Input{}, "", errors.New("query is required for search")
		}
		if len(input.Query) > 16<<10 {
			return Input{}, "", errors.New("query cannot exceed 16384 bytes")
		}
		if err := rejectFields(input, "destination", input.Destination != "", "content", input.Content != "", "patches", len(input.Patches) != 0, "max_bytes", input.MaxBytes != 0, "create_parents", input.CreateParents, "overwrite", input.Overwrite, "must_not_exist", input.MustNotExist, "dry_run", input.DryRun); err != nil {
			return Input{}, "", err
		}
		if input.SearchMode == "" {
			input.SearchMode = SearchContents
		}
		if input.SearchMode != SearchContents && input.SearchMode != SearchNames {
			return Input{}, "", errors.New("search_mode must be contents or names")
		}
		if input.MaxResults == 0 {
			input.MaxResults = e.maxSearchResults
		}
		if input.MaxResults < 1 || input.MaxResults > e.maxSearchResults {
			return Input{}, "", fmt.Errorf("max_results must be between 1 and %d", e.maxSearchResults)
		}
		if input.Regex {
			pattern := input.Query
			if !input.CaseSensitive {
				pattern = "(?i)" + pattern
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return Input{}, "", fmt.Errorf("compile search expression: %w", err)
			}
		}
	case OperationWrite:
		if err := rejectFields(input, "destination", input.Destination != "", "patches", len(input.Patches) != 0, "query", input.Query != "", "recursive", input.Recursive, "max_bytes", input.MaxBytes != 0, "max_results", input.MaxResults != 0, "overwrite", input.Overwrite); err != nil {
			return Input{}, "", err
		}
		content, err := decodeContent(input.Content, input.Encoding)
		if err != nil {
			return Input{}, "", err
		}
		if int64(len(content)) > e.maxWriteBytes {
			return Input{}, "", fmt.Errorf("%w: write is %d bytes; limit is %d", ErrTooLarge, len(content), e.maxWriteBytes)
		}
	case OperationPatch:
		if len(input.Patches) == 0 || len(input.Patches) > maxPatchEdits {
			return Input{}, "", fmt.Errorf("patches must contain between 1 and %d exact edits", maxPatchEdits)
		}
		if input.Encoding != EncodingUTF8 {
			return Input{}, "", errors.New("patch only supports utf8 files")
		}
		if err := rejectFields(input, "destination", input.Destination != "", "content", input.Content != "", "query", input.Query != "", "recursive", input.Recursive, "max_bytes", input.MaxBytes != 0, "max_results", input.MaxResults != 0, "overwrite", input.Overwrite, "must_not_exist", input.MustNotExist); err != nil {
			return Input{}, "", err
		}
		for index := range input.Patches {
			if input.Patches[index].OldText == "" {
				return Input{}, "", fmt.Errorf("patch %d old_text cannot be empty", index)
			}
			if input.Patches[index].ExpectedOccurrences == 0 {
				input.Patches[index].ExpectedOccurrences = 1
			}
			if input.Patches[index].ExpectedOccurrences < 1 || input.Patches[index].ExpectedOccurrences > 10_000 {
				return Input{}, "", fmt.Errorf("patch %d expected_occurrences must be between 1 and 10000", index)
			}
			if len(input.Patches[index].OldText) > int(e.maxWriteBytes) || len(input.Patches[index].NewText) > int(e.maxWriteBytes) {
				return Input{}, "", fmt.Errorf("%w: patch %d text exceeds %d bytes", ErrTooLarge, index, e.maxWriteBytes)
			}
		}
	case OperationMove:
		if input.Destination == "" {
			return Input{}, "", errors.New("destination is required for move")
		}
		if len(input.Destination) > 4_096 {
			return Input{}, "", errors.New("destination cannot exceed 4096 bytes")
		}
		if _, _, _, err := e.resolve(input.Destination); err != nil {
			return Input{}, "", fmt.Errorf("validate destination: %w", err)
		}
		if err := rejectFields(input, "content", input.Content != "", "patches", len(input.Patches) != 0, "query", input.Query != "", "recursive", input.Recursive, "max_bytes", input.MaxBytes != 0, "max_results", input.MaxResults != 0, "create_parents", input.CreateParents, "must_not_exist", input.MustNotExist); err != nil {
			return Input{}, "", err
		}
	case OperationDelete:
		if err := rejectFields(input, "destination", input.Destination != "", "content", input.Content != "", "patches", len(input.Patches) != 0, "query", input.Query != "", "max_bytes", input.MaxBytes != 0, "max_results", input.MaxResults != 0, "create_parents", input.CreateParents, "overwrite", input.Overwrite, "must_not_exist", input.MustNotExist); err != nil {
			return Input{}, "", err
		}
	default:
		return Input{}, "", fmt.Errorf("unsupported filesystem operation %q", input.Operation)
	}
	return input, Classify(input), nil
}

func validateOperationFields(raw json.RawMessage, operation Operation) error {
	common := []string{"operation", "path"}
	var specific []string
	switch operation {
	case OperationRead:
		specific = []string{"encoding", "max_bytes"}
	case OperationList:
		specific = []string{"recursive", "max_results"}
	case OperationSearch:
		specific = []string{"query", "regex", "case_sensitive", "search_mode", "recursive", "max_results"}
	case OperationWrite:
		specific = []string{"content", "encoding", "expected_sha256", "must_not_exist", "create_parents", "dry_run"}
	case OperationPatch:
		specific = []string{"patches", "expected_sha256", "dry_run"}
	case OperationMove:
		specific = []string{"destination", "expected_sha256", "overwrite", "dry_run"}
	case OperationDelete:
		specific = []string{"expected_sha256", "recursive", "dry_run"}
	default:
		return fmt.Errorf("unsupported filesystem operation %q", operation)
	}
	allowed := make(map[string]bool, len(common)+len(specific))
	for _, name := range append(common, specific...) {
		allowed[name] = true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("decode filesystem fields: %w", err)
	}
	for name := range fields {
		if !allowed[name] {
			return fmt.Errorf("field %s is not valid for %s", name, operation)
		}
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("filesystem input must contain exactly one JSON value")
		}
		return fmt.Errorf("decode filesystem input: %w", err)
	}
	return nil
}

func rejectFields(_ Input, fields ...any) error {
	for index := 0; index < len(fields); index += 2 {
		if fields[index+1].(bool) {
			return fmt.Errorf("field %s is not valid for this operation", fields[index].(string))
		}
	}
	return nil
}

func (e *Executor) Execute(ctx context.Context, raw json.RawMessage, _ func(coretools.Progress)) (json.RawMessage, error) {
	input, risk, err := e.ParseAndValidate(raw)
	if err != nil {
		return nil, err
	}
	ws, relative, absolute, err := e.resolve(input.Path)
	if err != nil {
		return nil, err
	}
	output := Output{Operation: input.Operation, Workspace: ws.path, Path: absolute, Risk: risk, ApprovalRequired: RequiresApproval(input), DryRun: input.DryRun}

	switch input.Operation {
	case OperationRead:
		data, hash, _, readErr := readRegular(ctx, ws, relative, input.MaxBytes)
		if readErr != nil {
			return nil, readErr
		}
		if input.Encoding == EncodingUTF8 && !utf8.Valid(data) {
			return nil, errors.New("file is not valid utf8; request base64 encoding")
		}
		output.Content, output.Encoding = encodeContent(data, input.Encoding), input.Encoding
		output.SHA256, output.Bytes = hash, int64(len(data))
	case OperationList:
		output.Entries, output.Truncated, err = e.list(ctx, ws, relative, input.Recursive, input.MaxResults)
	case OperationSearch:
		output.Matches, output.FilesScanned, output.FilesSkipped, output.Truncated, err = e.search(ctx, ws, relative, input)
	case OperationWrite:
		var data []byte
		data, err = decodeContent(input.Content, input.Encoding)
		if err == nil {
			output.PreviousSHA256, output.SHA256, output.Bytes, err = e.write(ctx, ws, relative, data, input)
		}
	case OperationPatch:
		output.PreviousSHA256, output.SHA256, output.Bytes, err = e.patch(ctx, ws, relative, input)
	case OperationMove:
		var destinationWorkspace *workspace
		var destinationRelative string
		destinationWorkspace, destinationRelative, output.Destination, err = e.resolve(input.Destination)
		if err == nil {
			output.PreviousSHA256, output.SHA256, output.Bytes, err = e.move(ctx, ws, relative, destinationWorkspace, destinationRelative, input)
		}
	case OperationDelete:
		output.PreviousSHA256, output.Bytes, err = e.delete(ctx, ws, relative, input)
	}
	if err != nil {
		return nil, fmt.Errorf("filesystem %s %q: %w", input.Operation, absolute, err)
	}
	return json.Marshal(output)
}

func (e *Executor) resolve(value string) (*workspace, string, string, error) {
	if value == "" || strings.ContainsRune(value, 0) {
		return nil, "", "", errors.New("path is required and cannot contain NUL")
	}
	if filepath.IsAbs(value) {
		cleaned := filepath.Clean(value)
		for index := range e.workspaces {
			for _, alias := range e.workspaces[index].aliases {
				relative, err := filepath.Rel(alias, cleaned)
				if err == nil && (relative == "." || filepath.IsLocal(relative)) {
					return &e.workspaces[index], relative, filepath.Join(e.workspaces[index].path, relative), nil
				}
			}
		}
		return nil, "", "", ErrOutsideWorkspace
	}
	if !filepath.IsLocal(value) {
		return nil, "", "", ErrOutsideWorkspace
	}
	relative := filepath.Clean(value)
	return &e.workspaces[0], relative, filepath.Join(e.workspaces[0].path, relative), nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func readRegular(ctx context.Context, ws *workspace, relative string, limit int64) ([]byte, string, fs.FileMode, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", 0, err
	}
	file, err := ws.root.Open(relative)
	if err != nil {
		return nil, "", 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, "", 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, "", 0, ErrUnsupportedType
	}
	if info.Size() > limit {
		return nil, "", 0, fmt.Errorf("%w: file is %d bytes; limit is %d", ErrTooLarge, info.Size(), limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, "", 0, err
	}
	if int64(len(data)) > limit {
		return nil, "", 0, fmt.Errorf("%w: file grew beyond %d bytes", ErrTooLarge, limit)
	}
	return data, sum(data), info.Mode().Perm(), nil
}

func decodeContent(content string, encoding Encoding) ([]byte, error) {
	if encoding == EncodingBase64 {
		data, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("decode base64 content: %w", err)
		}
		return data, nil
	}
	if !utf8.ValidString(content) {
		return nil, errors.New("utf8 content contains invalid encoding")
	}
	return []byte(content), nil
}

func encodeContent(content []byte, encoding Encoding) string {
	if encoding == EncodingBase64 {
		return base64.StdEncoding.EncodeToString(content)
	}
	return string(content)
}

func sum(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func typeName(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return "file"
	case mode.IsDir():
		return "directory"
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	default:
		return "other"
	}
}

func (e *Executor) list(ctx context.Context, ws *workspace, relative string, recursive bool, limit int) ([]Entry, bool, error) {
	var result []Entry
	truncated := false
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		handle, err := ws.root.Open(directory)
		if err != nil {
			return err
		}
		remaining := limit - len(result)
		entries, readErr := handle.ReadDir(remaining + 1)
		closeErr := handle.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, item := range entries {
			if len(result) >= limit {
				truncated = true
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			child := filepath.Join(directory, item.Name())
			info, err := ws.root.Lstat(child)
			if err != nil {
				return err
			}
			result = append(result, Entry{Path: filepath.Join(ws.path, child), Name: item.Name(), Type: typeName(info.Mode()), Size: info.Size(), Mode: info.Mode().String(), ModifiedAt: info.ModTime().UTC()})
			if recursive && info.IsDir() {
				if err := walk(child); err != nil {
					return err
				}
				if truncated {
					return nil
				}
			}
		}
		return nil
	}
	if err := walk(relative); err != nil {
		return nil, false, err
	}
	return result, truncated, nil
}

func (e *Executor) search(ctx context.Context, ws *workspace, relative string, input Input) ([]Match, int, int, bool, error) {
	matcher, err := newMatcher(input)
	if err != nil {
		return nil, 0, 0, false, err
	}
	var result []Match
	filesScanned, filesSkipped := 0, 0
	entriesVisited := 0
	truncated := false

	var inspect func(string, fs.FileInfo) error
	inspect = func(path string, info fs.FileInfo) error {
		if len(result) >= input.MaxResults || filesScanned+filesSkipped >= e.maxSearchFiles {
			truncated = true
			return nil
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil
		}
		if input.SearchMode == SearchNames {
			filesScanned++
			if start, ok := matcher(filepath.Base(path)); ok {
				result = append(result, Match{Path: filepath.Join(ws.path, path), Column: start + 1, Snippet: filepath.Base(path)})
			}
			return nil
		}
		if info.Size() > e.maxSearchFileBytes {
			filesSkipped++
			return nil
		}
		data, _, _, readErr := readRegular(ctx, ws, path, e.maxSearchFileBytes)
		if readErr != nil {
			if errors.Is(readErr, ErrTooLarge) {
				filesSkipped++
				return nil
			}
			return readErr
		}
		filesScanned++
		if !utf8.Valid(data) {
			filesSkipped++
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for lineIndex, line := range lines {
			remaining := line
			baseColumn := 0
			for {
				start, ok := matcher(remaining)
				if !ok {
					break
				}
				result = append(result, Match{Path: filepath.Join(ws.path, path), Line: lineIndex + 1, Column: baseColumn + start + 1, Snippet: truncateSnippet(line)})
				if len(result) >= input.MaxResults {
					truncated = true
					return nil
				}
				advance := start + 1
				if advance > len(remaining) {
					break
				}
				baseColumn += advance
				remaining = remaining[advance:]
			}
		}
		return nil
	}

	info, err := ws.root.Lstat(relative)
	if err != nil {
		return nil, 0, 0, false, err
	}
	if !info.IsDir() {
		if err := inspect(relative, info); err != nil {
			return nil, 0, 0, false, err
		}
		return result, filesScanned, filesSkipped, truncated, nil
	}
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := e.maxSearchFiles - entriesVisited
		if remaining <= 0 {
			truncated = true
			return nil
		}
		handle, err := ws.root.Open(directory)
		if err != nil {
			return err
		}
		children, readErr := handle.ReadDir(remaining + 1)
		closeErr := handle.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		overflow := len(children) > remaining
		if overflow {
			children = children[:remaining]
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, child := range children {
			entriesVisited++
			if truncated {
				return nil
			}
			path := filepath.Join(directory, child.Name())
			childInfo, err := ws.root.Lstat(path)
			if err != nil {
				return err
			}
			if childInfo.IsDir() {
				if input.Recursive {
					if err := walk(path); err != nil {
						return err
					}
				}
				continue
			}
			if err := inspect(path, childInfo); err != nil {
				return err
			}
		}
		if overflow {
			truncated = true
		}
		return nil
	}
	if err := walk(relative); err != nil {
		return nil, 0, 0, false, err
	}
	return result, filesScanned, filesSkipped, truncated, nil
}

func newMatcher(input Input) (func(string) (int, bool), error) {
	if input.Regex {
		pattern := input.Query
		if !input.CaseSensitive {
			pattern = "(?i)" + pattern
		}
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		return func(value string) (int, bool) {
			location := expression.FindStringIndex(value)
			return func() int {
				if location == nil {
					return 0
				}
				return location[0]
			}(), location != nil
		}, nil
	}
	needle := input.Query
	return func(value string) (int, bool) {
		candidate := value
		query := needle
		if !input.CaseSensitive {
			candidate, query = strings.ToLower(candidate), strings.ToLower(query)
		}
		index := strings.Index(candidate, query)
		return index, index >= 0
	}, nil
}

func truncateSnippet(value string) string {
	const limit = 512
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func (e *Executor) existingHash(ctx context.Context, ws *workspace, relative string) (string, int64, fs.FileMode, bool, error) {
	info, err := ws.root.Lstat(relative)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, e.writeMode, false, nil
	}
	if err != nil {
		return "", 0, 0, false, err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", 0, 0, true, ErrUnsupportedType
	}
	data, hash, mode, err := readRegular(ctx, ws, relative, e.maxHashBytes)
	return hash, int64(len(data)), mode, true, err
}

func checkExpected(actual string, exists bool, input Input) error {
	if input.MustNotExist && exists {
		return fmt.Errorf("%w: target already exists", ErrConflict)
	}
	if input.ExpectedSHA256 != "" && (!exists || actual != input.ExpectedSHA256) {
		return fmt.Errorf("%w: expected sha256 %s, found %s", ErrConflict, input.ExpectedSHA256, actual)
	}
	return nil
}

func (e *Executor) write(ctx context.Context, ws *workspace, relative string, data []byte, input Input) (string, string, int64, error) {
	previous, _, mode, exists, err := e.existingHash(ctx, ws, relative)
	if err != nil {
		return "", "", 0, err
	}
	if err := checkExpected(previous, exists, input); err != nil {
		return "", "", 0, err
	}
	if input.DryRun {
		return previous, sum(data), int64(len(data)), nil
	}
	if input.CreateParents {
		if err := ws.root.MkdirAll(filepath.Dir(relative), 0o750); err != nil {
			return "", "", 0, err
		}
	}
	if err := atomicWrite(ctx, ws, relative, data, mode); err != nil {
		return "", "", 0, err
	}
	return previous, sum(data), int64(len(data)), nil
}

func (e *Executor) patch(ctx context.Context, ws *workspace, relative string, input Input) (string, string, int64, error) {
	data, previous, mode, err := readRegular(ctx, ws, relative, e.maxWriteBytes)
	if err != nil {
		return "", "", 0, err
	}
	if input.ExpectedSHA256 != "" && previous != input.ExpectedSHA256 {
		return "", "", 0, fmt.Errorf("%w: expected sha256 %s, found %s", ErrConflict, input.ExpectedSHA256, previous)
	}
	if !utf8.Valid(data) {
		return "", "", 0, errors.New("patch target is not valid utf8")
	}
	updated := append([]byte(nil), data...)
	for index, patch := range input.Patches {
		count := bytes.Count(updated, []byte(patch.OldText))
		if count != patch.ExpectedOccurrences {
			return "", "", 0, fmt.Errorf("%w: patch %d expected %d occurrences, found %d", ErrConflict, index, patch.ExpectedOccurrences, count)
		}
		updated = bytes.ReplaceAll(updated, []byte(patch.OldText), []byte(patch.NewText))
		if int64(len(updated)) > e.maxWriteBytes {
			return "", "", 0, fmt.Errorf("%w: patched content exceeds %d bytes", ErrTooLarge, e.maxWriteBytes)
		}
	}
	if input.DryRun {
		return previous, sum(updated), int64(len(updated)), nil
	}
	if err := atomicWrite(ctx, ws, relative, updated, mode); err != nil {
		return "", "", 0, err
	}
	return previous, sum(updated), int64(len(updated)), nil
}

func atomicWrite(ctx context.Context, ws *workspace, relative string, data []byte, mode fs.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Dir(relative)
	base := filepath.Base(relative)
	var temporary string
	var file *os.File
	for attempt := 0; attempt < 10; attempt++ {
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		temporary = filepath.Join(parent, "."+base+".veqri-"+hex.EncodeToString(nonce[:]))
		var err error
		file, err = ws.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if file == nil {
		return errors.New("could not allocate a temporary file")
	}
	cleanup := func() { _ = ws.root.Remove(temporary) }
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		cleanup()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return err
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return err
	}
	if err := ws.root.Rename(temporary, relative); err != nil {
		cleanup()
		return err
	}
	if directory, err := ws.root.Open(parent); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func (e *Executor) move(ctx context.Context, sourceWorkspace *workspace, source string, destinationWorkspace *workspace, destination string, input Input) (string, string, int64, error) {
	if sourceWorkspace != destinationWorkspace {
		return "", "", 0, errors.New("moves between workspace roots are denied")
	}
	if source == "." || destination == "." {
		return "", "", 0, errors.New("moving a workspace root is denied")
	}
	if err := ctx.Err(); err != nil {
		return "", "", 0, err
	}
	sourceInfo, err := sourceWorkspace.root.Lstat(source)
	if err != nil {
		return "", "", 0, err
	}
	var hash string
	var size int64
	if sourceInfo.Mode().IsRegular() {
		_, hash, _, err = readRegular(ctx, sourceWorkspace, source, e.maxHashBytes)
		if err != nil {
			return "", "", 0, err
		}
		size = sourceInfo.Size()
	}
	destinationInfo, destinationErr := destinationWorkspace.root.Lstat(destination)
	if destinationErr == nil {
		if !input.Overwrite {
			return "", "", 0, fmt.Errorf("%w: destination already exists", ErrConflict)
		}
		if destinationInfo.IsDir() {
			return "", "", 0, errors.New("overwriting a directory is denied")
		}
	} else if !errors.Is(destinationErr, fs.ErrNotExist) {
		return "", "", 0, destinationErr
	}
	if input.ExpectedSHA256 != "" && hash != input.ExpectedSHA256 {
		return "", "", 0, fmt.Errorf("%w: expected sha256 %s, found %s", ErrConflict, input.ExpectedSHA256, hash)
	}
	if input.DryRun {
		return hash, hash, size, nil
	}
	if err := sourceWorkspace.root.Rename(source, destination); err != nil {
		return "", "", 0, err
	}
	return hash, hash, size, nil
}

func (e *Executor) delete(ctx context.Context, ws *workspace, relative string, input Input) (string, int64, error) {
	if relative == "." {
		return "", 0, errors.New("deleting a workspace root is denied")
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	info, err := ws.root.Lstat(relative)
	if err != nil {
		return "", 0, err
	}
	var hash string
	var size int64
	if info.Mode().IsRegular() {
		_, hash, _, err = readRegular(ctx, ws, relative, e.maxHashBytes)
		if err != nil {
			return "", 0, err
		}
		size = info.Size()
	}
	if input.ExpectedSHA256 != "" && hash != input.ExpectedSHA256 {
		return "", 0, fmt.Errorf("%w: expected sha256 %s, found %s", ErrConflict, input.ExpectedSHA256, hash)
	}
	if info.IsDir() && !input.Recursive {
		// Root.Remove below intentionally permits deleting only an empty directory.
	} else if !info.IsDir() && input.Recursive {
		return "", 0, errors.New("recursive is only valid when deleting a directory")
	}
	if input.DryRun {
		return hash, size, nil
	}
	if info.IsDir() && input.Recursive {
		err = ws.root.RemoveAll(relative)
	} else {
		err = ws.root.Remove(relative)
	}
	return hash, size, err
}
