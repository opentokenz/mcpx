// Package edit implements a lightweight, high-throughput file edit engine for
// the clean-core MCP surface.
package edit

import (
	"errors"
	"mcpx/internal/file"
)

// MaxChangedLines is the hard cap on total unified-diff changed lines
// (insertions + deletions) for one ApplyBatch call.
const MaxChangedLines = 1000

// Operation names for a single file edit.
const (
	OpCreate = "create"
	OpUpdate = "update"
	OpDelete = "delete"
	OpRename = "rename"
)

var (
	ErrStale          = errors.New("file revision is stale")
	ErrMatchNotFound  = errors.New("replacement match not found")
	ErrMatchAmbiguous = errors.New("replacement match is ambiguous")
	ErrTooManyChanges = errors.New("too many changed lines")
	ErrInvalidInput   = errors.New("invalid edit input")
	ErrUnsupportedOp  = errors.New("unsupported operation")
	ErrTargetExists   = errors.New("create target already exists")
	ErrTargetMissing  = errors.New("target does not exist")
)

// Replacement is one exact string substitution. Match must occur exactly once.
type Replacement struct {
	Match       string `json:"match"`
	Replacement string `json:"replacement"`
}

// LineRange replaces complete logical lines using 1-based inclusive indexes.
// It is only valid for revision-guarded updates.
type LineRange struct {
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	Replacement string `json:"replacement"`
}

// FileEdit is one path-level operation inside a batch.
type FileEdit struct {
	Path           string          `json:"path"`
	Operation      string          `json:"operation"`
	BaseSHA256     string          `json:"base_sha256,omitempty"`
	Revision       string          `json:"rev,omitempty"`
	Content        string          `json:"content,omitempty"`
	NewPath        string          `json:"new_path,omitempty"`
	Replacements   []Replacement   `json:"replacements,omitempty"`
	Range          *LineRange      `json:"range,omitempty"`
	ContentBase64  *string         `json:"content_base64,omitempty"`
	NewlinePolicy  string          `json:"newline_policy,omitempty"`
	ExpectedFormat *ExpectedFormat `json:"expected_format,omitempty"`
}

// ExpectedFormat 明确约束本次写入后的字节格式，不触发编码或换行转换。
type ExpectedFormat struct {
	Charset    string `json:"charset"`
	BOM        string `json:"bom"`
	LineEnding string `json:"line_ending"`
}

type ByteReadback struct {
	ByteLength int         `json:"byte_length"`
	SHA256     string      `json:"sha256"`
	Format     file.Format `json:"format"`
	TailHex    string      `json:"tail_hex"`
}

// BatchRequest is one edit tool call worth of work.
type BatchRequest struct {
	WorkspaceRoot string
	Edits         []FileEdit
	// DryRun validates and builds the exact result without invoking any
	// filesystem mutation or pre-write hook.
	DryRun bool
	// ValidatePath 检查实际物理路径，在准备及最终写入核对阶段均调用。
	ValidatePath func(absolutePath string) error
}

// FileResult is the outcome for one path.
type FileResult struct {
	Path           string        `json:"path"`
	NewPath        string        `json:"new_path,omitempty"`
	Operation      string        `json:"operation"`
	OriginalSHA256 string        `json:"original_sha256,omitempty"`
	NewSHA256      string        `json:"new_sha256,omitempty"`
	ChangedLines   int           `json:"changed_lines"`
	Diff           string        `json:"diff,omitempty"`
	Deleted        bool          `json:"deleted,omitempty"`
	Readback       *ByteReadback `json:"readback,omitempty"`
}

// BatchResult is the aggregate outcome.
type BatchResult struct {
	Results           []FileResult `json:"results"`
	TotalChangedLines int          `json:"total_changed_lines"`
	DiffSummary       string       `json:"diff_summary"`
}

// ApplyError carries structured recovery hints for tool handlers.
type ApplyError struct {
	Code    string
	Message string
	Path    string
	Index   int // replacement index within file, or -1
	Current string
	// ChangedLines is the exact cumulative +/- count when validation reaches
	// the batch limit. It stays zero for failures that occur before diffing.
	ChangedLines int
	Err          error
}

func (e *ApplyError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

func (e *ApplyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
