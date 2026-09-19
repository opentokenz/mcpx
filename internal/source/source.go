package source

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"mcpx/internal/file"
)

// LimitError is a request-level capacity violation. It is kept in the source
// package so list/read callers can expose the same machine-readable contract.
type LimitError struct {
	Resource string
	Actual   int
	Max      int
}

func (e *LimitError) Error() string {
	if e == nil {
		return "source request exceeds its limit"
	}
	return fmt.Sprintf("%s exceeds maximum of %d", e.Resource, e.Max)
}

type Entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

type ListResult struct {
	Files      []Entry `json:"files"`
	NextCursor string  `json:"next_cursor,omitempty"`
	Total      int     `json:"total"`
}

// DirectEntry is an immediate child of a requested source scope. Unlike
// ListResult.Files, it includes directories and symbolic links and never
// recursively expands them. It exists so an agent can establish a complete
// top-level workspace inventory before preparing a broad move-out operation.
type DirectEntry struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size,omitempty"`
}

type DirectListResult struct {
	Entries        []DirectEntry `json:"entries"`
	Total          int           `json:"total"`
	NextCursor     string        `json:"next_cursor,omitempty"`
	PolicyFiltered bool          `json:"policy_filtered,omitempty"`
}

type Match struct {
	Path   string   `json:"path"`
	Line   int      `json:"line"`
	Column int      `json:"column"`
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
	SHA256 string   `json:"sha256,omitempty"`
}

type SearchResult struct {
	Matches    []Match `json:"matches"`
	NextCursor string  `json:"next_cursor,omitempty"`
	Truncated  bool    `json:"truncated"`
}

type SearchOptions struct {
	Query          string
	Pattern        string
	ExcludePattern string
	ScopePaths     []string
	Cursor         string
	Regex          bool
	CaseSensitive  bool
	Limit          int
	ContextBefore  int
	ContextAfter   int
	IncludeSHA256  bool
}

type ReadResult struct {
	file.ReadResult
	SHA256 string `json:"sha256"`
	OK     bool   `json:"ok,omitempty"`
	Error  string `json:"error,omitempty"`
}

type BatchReadRequest struct {
	Path           string `json:"path"`
	Offset         int    `json:"offset"`
	LineByteOffset int    `json:"line_byte_offset,omitempty"`
	Limit          int    `json:"limit"`
}

type BatchReadResult struct {
	Results          []ReadResult       `json:"results"`
	TotalBytes       int                `json:"total_bytes"`
	Truncated        bool               `json:"truncated"`
	BudgetBytes      int                `json:"budget_bytes,omitempty"`
	ContinueFrom     int                `json:"continue_from,omitempty"`
	ContinueRequests []BatchReadRequest `json:"-"`
}

var ignoredDirectories = map[string]bool{
	".git": true, ".mcpx": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "bin": true, "target": true, ".next": true,
}

const MaxDirectListEntries = 500
const DefaultDirectListEntries = 100

func List(root, pattern, cursor string, limit int, includeHashes bool, allowed func(string) bool) (ListResult, error) {
	return ListWith(root, pattern, "", cursor, limit, includeHashes, allowed)
}

// ListDirect returns the direct children of a scope without walking child
// directories or following symbolic links. An empty scope means the workspace
// root. Entries are sorted by path, and policy-filtered entries are counted
// only through PolicyFiltered so callers never mistake a filtered inventory
// for a full filesystem listing.
func ListDirect(root, scope, cursor string, limit int, allowed func(string) bool) (DirectListResult, error) {
	if limit <= 0 || limit > MaxDirectListEntries {
		limit = DefaultDirectListEntries
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return DirectListResult{}, err
	}
	rootAbs = filepath.Clean(rootAbs)
	base := rootAbs
	scope = filepath.ToSlash(filepath.Clean(strings.TrimSpace(scope)))
	if scope == "." {
		scope = ""
	}
	if scope != "" {
		if _, err = file.Resolve(rootAbs, scope); err != nil {
			return DirectListResult{}, err
		}
		base, err = file.LexicalPath(rootAbs, scope)
		if err != nil {
			return DirectListResult{}, err
		}
		info, err := os.Lstat(base)
		if err != nil {
			return DirectListResult{}, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			entry := DirectEntry{Path: scope, Kind: directFileInfoKind(info)}
			if entry.Kind == "file" {
				entry.Size = info.Size()
			}
			if allowed != nil && !allowed(scope) {
				return DirectListResult{PolicyFiltered: true}, nil
			}
			return DirectListResult{Entries: []DirectEntry{entry}, Total: 1}, nil
		}
	}
	items, err := os.ReadDir(base)
	if err != nil {
		return DirectListResult{}, err
	}
	all := make([]DirectEntry, 0, len(items))
	policyFiltered := false
	for _, item := range items {
		relative := item.Name()
		if scope != "" {
			relative = filepath.ToSlash(filepath.Join(scope, item.Name()))
		}
		if allowed != nil && !allowed(relative) && !allowed(relative+"/") {
			policyFiltered = true
			continue
		}
		entry := DirectEntry{Path: filepath.ToSlash(relative), Kind: directEntryKind(item)}
		if entry.Kind == "file" {
			if info, infoErr := item.Info(); infoErr == nil {
				entry.Size = info.Size()
			}
		}
		all = append(all, entry)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
	start := decodeCursor(cursor)
	if start > len(all) {
		start = len(all)
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	result := DirectListResult{Entries: append([]DirectEntry(nil), all[start:end]...), Total: len(all), PolicyFiltered: policyFiltered}
	if end < len(all) {
		result.NextCursor = encodeCursor(end)
	}
	return result, nil
}

func directEntryKind(item os.DirEntry) string {
	mode := item.Type()
	switch {
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case item.IsDir():
		return "directory"
	case mode.IsRegular():
		return "file"
	default:
		return "other"
	}
}

func directFileInfoKind(info os.FileInfo) string {
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case info.IsDir():
		return "directory"
	case mode.IsRegular():
		return "file"
	default:
		return "other"
	}
}

// ListWith adds a server-side exclude glob so callers stop walking once their
// requested result budget is met instead of loading and filtering a full page.
func ListWith(root, pattern, excludePattern, cursor string, limit int, includeHashes bool, allowed func(string) bool) (ListResult, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	paths, err := paths(root, pattern, excludePattern, allowed)
	if err != nil {
		return ListResult{}, err
	}
	start := decodeCursor(cursor)
	if start > len(paths) {
		start = len(paths)
	}
	end := start + limit
	if end > len(paths) {
		end = len(paths)
	}
	result := ListResult{Total: len(paths)}
	for _, path := range paths[start:end] {
		absolute, _ := file.Resolve(root, path)
		info, err := os.Stat(absolute)
		if err != nil {
			continue
		}
		entry := Entry{Path: path, Size: info.Size()}
		if includeHashes && info.Size() <= 4<<20 {
			content, err := os.ReadFile(absolute)
			if err == nil {
				entry.SHA256 = digest(content)
			}
		}
		result.Files = append(result.Files, entry)
	}
	if end < len(paths) {
		result.NextCursor = encodeCursor(end)
	}
	return result, nil
}

func Search(root, query, pattern, cursor string, regex bool, limit int, allowed func(string) bool) (SearchResult, error) {
	return SearchWith(root, SearchOptions{
		Query: query, Pattern: pattern, Cursor: cursor, Regex: regex, CaseSensitive: true, Limit: limit,
	}, allowed)
}

func SearchWith(root string, opts SearchOptions, allowed func(string) bool) (SearchResult, error) {
	if opts.Query == "" {
		return SearchResult{}, fmt.Errorf("query required")
	}
	if opts.Limit <= 0 || opts.Limit > 500 {
		opts.Limit = 100
	}
	if opts.ContextBefore < 0 {
		opts.ContextBefore = 0
	}
	if opts.ContextBefore > 20 {
		opts.ContextBefore = 20
	}
	if opts.ContextAfter < 0 {
		opts.ContextAfter = 0
	}
	if opts.ContextAfter > 20 {
		opts.ContextAfter = 20
	}
	var expression *regexp.Regexp
	var err error
	if opts.Regex {
		expression, err = regexp.Compile(opts.Query)
		if err != nil {
			return SearchResult{}, fmt.Errorf("invalid regular expression: %w", err)
		}
	}
	if len(opts.ScopePaths) > 0 {
		allowed = ScopePathFilter(root, opts.ScopePaths, allowed)
	}
	filePaths, err := paths(root, opts.Pattern, opts.ExcludePattern, allowed)
	if err != nil {
		return SearchResult{}, err
	}
	start := decodeCursor(opts.Cursor)
	seen := 0
	page := make([]Match, 0, opts.Limit+1)
	complete := false
	queryBytes := []byte(opts.Query)
	for _, path := range filePaths {
		absolute, _ := file.Resolve(root, path)
		info, err := os.Stat(absolute)
		if err != nil || info.Size() > 2<<20 {
			continue
		}
		content, err := os.ReadFile(absolute)
		if err != nil || !utf8.Valid(content) {
			continue
		}
		// 字面量未命中时无需分行；忽略大小写和正则仍使用原匹配语义。
		if !opts.Regex && opts.CaseSensitive && !bytes.Contains(content, queryBytes) {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		var fileSHA string
		for lineNumber, line := range lines {
			var indices [][]int
			if opts.Regex {
				indices = expression.FindAllStringIndex(line, -1)
			} else {
				indices = literalIndicesWithCase(line, opts.Query, opts.CaseSensitive)
			}
			for _, index := range indices {
				seen++
				if seen <= start {
					continue
				}
				// 每个实际返回匹配的文件只计算一次原始字节哈希。
				if opts.IncludeSHA256 && fileSHA == "" {
					fileSHA = digest(content)
				}
				text := line
				if len(text) > 500 {
					text = text[:500]
				}
				match := Match{Path: path, Line: lineNumber + 1, Column: index[0] + 1, Text: text, SHA256: fileSHA}
				if opts.ContextBefore > 0 {
					from := lineNumber - opts.ContextBefore
					if from < 0 {
						from = 0
					}
					match.Before = append([]string{}, lines[from:lineNumber]...)
				}
				if opts.ContextAfter > 0 {
					to := lineNumber + 1 + opts.ContextAfter
					if to > len(lines) {
						to = len(lines)
					}
					if lineNumber+1 < to {
						match.After = append([]string{}, lines[lineNumber+1:to]...)
					}
				}
				page = append(page, match)
				if len(page) > opts.Limit {
					complete = true
					break
				}
			}
			if complete {
				break
			}
		}
		if complete {
			break
		}
	}
	result := SearchResult{Matches: page}
	if len(page) > opts.Limit {
		result.Matches = page[:opts.Limit]
		result.NextCursor, result.Truncated = encodeCursor(start+opts.Limit), true
	}
	return result, nil
}

func Read(root, path string, offset, limit int, maxBytes int64) (ReadResult, error) {
	return ReadWindow(root, path, offset, 0, limit, maxBytes)
}

func ReadWindow(root, path string, offset, lineByteOffset, limit int, maxBytes int64) (ReadResult, error) {
	result, err := file.Read(file.ReadOptions{WorkspaceRoot: root, Path: path, Offset: offset, LineByteOffset: lineByteOffset, Limit: limit, MaxBytes: maxBytes})
	if err != nil {
		return ReadResult{}, err
	}
	return ReadResult{ReadResult: result, SHA256: result.SHA256, OK: true}, nil
}

// ReadBatch reads multiple windows. One failure does not abort the batch.
// budgetBytes caps total content size across successful reads (0 = 1 MiB default).
func ReadBatch(root string, requests []BatchReadRequest, maxBytesPerFile int64, budgetBytes int, allowed func(string) bool) BatchReadResult {
	if budgetBytes <= 0 {
		budgetBytes = 1 << 20
	}
	if maxBytesPerFile <= 0 {
		maxBytesPerFile = 1 << 20
	}
	out := BatchReadResult{Results: make([]ReadResult, 0, len(requests)), BudgetBytes: budgetBytes}
	continuations := make([]BatchReadRequest, 0)
	continuationFrom := -1
	addContinuation := func(index int, request BatchReadRequest) {
		if continuationFrom < 0 {
			continuationFrom = index
		}
		continuations = append(continuations, request)
	}
	for index, request := range requests {
		if allowed != nil && !allowed(request.Path) {
			out.Results = append(out.Results, ReadResult{
				ReadResult: file.ReadResult{Path: request.Path},
				OK:         false,
				Error:      "file denied by policy",
			})
			continue
		}
		if out.TotalBytes >= budgetBytes {
			if continuationFrom < 0 {
				continuationFrom = index
			}
			continuations = append(continuations, requests[index:]...)
			break
		}
		result, err := ReadWindow(root, request.Path, request.Offset, request.LineByteOffset, request.Limit, maxBytesPerFile)
		if err != nil {
			out.Results = append(out.Results, ReadResult{
				ReadResult: file.ReadResult{Path: request.Path, Offset: request.Offset, Limit: request.Limit},
				OK:         false,
				Error:      err.Error(),
			})
			continue
		}
		remain := budgetBytes - out.TotalBytes
		if len(result.Content) > remain {
			result.Content = truncateUTF8(result.Content, remain)
			result.Truncated = true
			out.TotalBytes += len(result.Content)
			out.Results = append(out.Results, result)
			continuation := advanceBatchReadRequest(request, result.Content)
			addContinuation(index, continuation)
			continuations = append(continuations, requests[index+1:]...)
			break
		}
		out.TotalBytes += len(result.Content)
		out.Results = append(out.Results, result)
		if result.Truncated {
			continuation := advanceBatchReadRequest(request, result.Content)
			addContinuation(index, continuation)
		}
	}
	if len(continuations) > 0 {
		out.setContinuations(continuationFrom, continuations)
	}
	return out
}

func (result *BatchReadResult) setContinuations(index int, requests []BatchReadRequest) {
	result.Truncated = true
	result.ContinueFrom = index
	result.ContinueRequests = append(result.ContinueRequests[:0], requests...)
}

func AdvanceReadCursor(offset, lineByteOffset int, content string) (int, int) {
	completedLines := strings.Count(content, "\n")
	if completedLines == 0 {
		return offset, lineByteOffset + len(content)
	}
	lastNewline := strings.LastIndexByte(content, '\n')
	return offset + completedLines, len(content) - lastNewline - 1
}

func advanceBatchReadRequest(request BatchReadRequest, content string) BatchReadRequest {
	request.Offset, request.LineByteOffset = AdvanceReadCursor(request.Offset, request.LineByteOffset, content)
	return request
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	for maxBytes > 0 && !utf8.RuneStart(value[maxBytes]) {
		maxBytes--
	}
	return value[:maxBytes]
}

// Query assembles the first page of intent-oriented context from search hits and seed paths.
func Query(root, intent string, seeds []string, maxFiles, contextBefore, contextAfter int, maxBytesPerFile int64, allowed func(string) bool) (map[string]any, error) {
	return QueryPage(root, intent, seeds, maxFiles, contextBefore, contextAfter, maxBytesPerFile, "", allowed)
}

// QueryPage is the cursor-aware form used by the MCP facade. The cursor is an
// offset in the stable, score-sorted candidate list for this request.
func QueryPage(root, intent string, seeds []string, maxFiles, contextBefore, contextAfter int, maxBytesPerFile int64, cursor string, allowed func(string) bool) (map[string]any, error) {
	if maxFiles <= 0 || maxFiles > 20 {
		maxFiles = 10
	}
	if maxBytesPerFile <= 0 {
		maxBytesPerFile = 64 << 10
	}
	type scored struct {
		path       string
		score      int
		why        []string
		searchHits int
	}
	scores := map[string]*scored{}
	implementationIntent := isImplementationIntent(intent)
	expandedSeeds, scopePrefixes := expandSeedPaths(root, seeds, allowed)
	queryAllowed := allowed
	if len(scopePrefixes) > 0 {
		queryAllowed = func(path string) bool {
			if allowed != nil && !allowed(path) {
				return false
			}
			path = filepath.ToSlash(path)
			for _, prefix := range scopePrefixes {
				if prefix == "" || path == prefix || strings.HasPrefix(path, prefix+"/") {
					return true
				}
			}
			return false
		}
	}
	add := func(path, reason string, points int) {
		if path == "" {
			return
		}
		if queryAllowed != nil && !queryAllowed(path) {
			return
		}
		item, ok := scores[path]
		if !ok {
			item = &scored{path: path}
			if implementationIntent && reason != "seed_path" {
				bias, biasReason := implementationPathBias(path)
				item.score += bias
				if biasReason != "" {
					item.why = append(item.why, biasReason)
				}
			}
			scores[path] = item
		}
		if strings.HasPrefix(reason, "search_hit:") {
			item.searchHits++
			if item.searchHits > 3 {
				return
			}
		}
		item.score += points
		item.why = append(item.why, reason)
	}
	for _, seed := range expandedSeeds {
		add(filepath.ToSlash(seed), "seed_path", 50)
	}
	terms := make([]string, 0, len(strings.Fields(intent)))
	for _, term := range strings.Fields(intent) {
		if len(term) >= 3 {
			terms = append(terms, regexp.QuoteMeta(term))
		}
	}
	if len(terms) > 0 {
		// Search once for all intent terms. The previous implementation scanned
		// every file once for the full intent and once per term, multiplying I/O
		// for ordinary natural-language requests.
		search, err := SearchWith(root, SearchOptions{
			Query: "(?:" + strings.Join(terms, "|") + ")", Regex: true,
			Limit: maxFiles * 3, ContextBefore: contextBefore, ContextAfter: contextAfter,
		}, queryAllowed)
		if err == nil {
			for _, match := range search.Matches {
				add(match.Path, fmt.Sprintf("search_hit:%d", match.Line), 10)
			}
		}
	}
	ordered := make([]*scored, 0, len(scores))
	for _, item := range scores {
		ordered = append(ordered, item)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].score == ordered[j].score {
			return ordered[i].path < ordered[j].path
		}
		return ordered[i].score > ordered[j].score
	})
	positive := ordered[:0]
	for _, item := range ordered {
		if item.score >= 0 {
			positive = append(positive, item)
		}
	}
	ordered = positive
	start := decodeCursor(cursor)
	if start > len(ordered) {
		start = len(ordered)
	}
	end := start + maxFiles
	if end > len(ordered) {
		end = len(ordered)
	}
	page := ordered[start:end]
	files := make([]map[string]any, 0, len(page))
	var totalBytes int
	for _, item := range page {
		read, err := Read(root, item.path, 0, 120, maxBytesPerFile)
		entry := map[string]any{
			"path": item.path, "score": item.score, "reasons": item.why,
		}
		if err != nil {
			entry["ok"] = false
			entry["error"] = err.Error()
		} else {
			entry["ok"] = true
			entry["content"] = read.Content
			entry["sha256"] = read.SHA256
			entry["offset"] = read.Offset
			entry["limit"] = read.Limit
			entry["total_lines"] = read.TotalLines
			entry["truncated"] = read.Truncated
			totalBytes += len(read.Content)
		}
		// Shallow import expansion for Go files.
		if strings.HasSuffix(item.path, ".go") && err == nil {
			imports := shallowGoImports(read.Content)
			entry["imports"] = imports
		}
		files = append(files, entry)
	}
	result := map[string]any{
		"query": intent, "files": files, "total_bytes": totalBytes,
		"truncated": end < len(ordered), "max_results": maxFiles,
	}
	if end < len(ordered) {
		result["next_cursor"] = encodeCursor(end)
	}
	return result, nil
}

func expandSeedPaths(root string, seeds []string, allowed func(string) bool) ([]string, []string) {
	expanded := make([]string, 0, len(seeds))
	scopes := make([]string, 0)
	var allPaths []string
	loadedPaths := false
	for _, seed := range seeds {
		normalized := filepath.ToSlash(filepath.Clean(seed))
		absolute, err := file.Resolve(root, seed)
		if err != nil {
			expanded = append(expanded, seed)
			continue
		}
		info, err := os.Stat(absolute)
		if err != nil || !info.IsDir() {
			expanded = append(expanded, seed)
			continue
		}
		prefix := strings.TrimSuffix(normalized, "/")
		if prefix == "." {
			prefix = ""
		}
		scopes = append(scopes, prefix)
		if !loadedPaths {
			allPaths, _ = paths(root, "", "", allowed)
			loadedPaths = true
		}
		for _, candidate := range allPaths {
			if prefix == "" || strings.HasPrefix(candidate, prefix+"/") {
				expanded = append(expanded, candidate)
			}
		}
	}
	return expanded, scopes
}

func isImplementationIntent(intent string) bool {
	intent = strings.ToLower(intent)
	for _, marker := range []string{"实现", "代码", "检查", "验证", "implementation", "implement", "source", "code", "verify", "atomic", "default", "wait"} {
		if strings.Contains(intent, marker) {
			return true
		}
	}
	return false
}

func implementationPathBias(path string) (int, string) {
	path = strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(path)
	switch {
	case strings.HasPrefix(path, "docs/plans/"), strings.HasPrefix(path, "docs/specs/"):
		return -40, "plan_or_spec"
	case base == "readme.md", base == "agents.md", base == "claude.md", base == "codex.md":
		return -30, "project_document"
	case strings.HasSuffix(path, "_test.go"), strings.Contains(path, "/test/"):
		return 20, "test"
	case strings.HasPrefix(path, "internal/"), strings.HasPrefix(path, "cmd/"),
		strings.HasSuffix(path, ".go"), strings.HasSuffix(path, ".ts"), strings.HasSuffix(path, ".tsx"),
		strings.HasSuffix(path, ".js"), strings.HasSuffix(path, ".jsx"):
		return 35, "implementation"
	default:
		return 0, ""
	}
}

func shallowGoImports(content string) []string {
	var imports []string
	lines := strings.Split(content, "\n")
	inBlock := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "import (") {
			inBlock = true
			continue
		}
		if inBlock {
			if trim == ")" {
				inBlock = false
				continue
			}
			trim = strings.Trim(trim, `"`)
			if trim != "" && !strings.HasPrefix(trim, "//") {
				imports = append(imports, trim)
			}
			continue
		}
		if strings.HasPrefix(trim, `import "`) {
			imports = append(imports, strings.Trim(strings.TrimPrefix(trim, "import "), `"`))
		}
	}
	return imports
}

// MatchGlob matches workspace-relative slash-separated paths. It preserves
// filepath.Match semantics for ordinary patterns and additionally supports the
// recursive ** wildcard used by MCP include/exclude globs.
func MatchGlob(pattern, value string) (bool, error) {
	pattern = filepath.ToSlash(pattern)
	value = filepath.ToSlash(value)
	if !strings.Contains(pattern, "**") {
		return filepath.Match(pattern, value)
	}
	var expression strings.Builder
	expression.WriteString("^")
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				for index+1 < len(pattern) && pattern[index+1] == '*' {
					index++
				}
				if index+1 < len(pattern) && pattern[index+1] == '/' {
					index++
					expression.WriteString("(?:.*/)?")
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(pattern[index+1:], ']')
			if end < 0 {
				return false, fmt.Errorf("invalid pattern: unterminated character class")
			}
			end += index + 1
			class := pattern[index : end+1]
			if len(class) > 2 && class[1] == '!' {
				class = "[^" + class[2:]
			}
			expression.WriteString(class)
			index = end
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
		}
	}
	expression.WriteString("$")
	compiled, err := regexp.Compile(expression.String())
	if err != nil {
		return false, fmt.Errorf("invalid pattern: %w", err)
	}
	return compiled.MatchString(value), nil
}

func paths(root, pattern, excludePattern string, allowed func(string) bool) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var result []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			if ignoredDirectories[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if allowed != nil && !allowed(relative) {
			return nil
		}
		if pattern != "" {
			matched, err := MatchGlob(pattern, relative)
			if err != nil {
				return fmt.Errorf("invalid pattern: %w", err)
			}
			if !matched {
				return nil
			}
		}
		if excludePattern != "" {
			excluded, err := MatchGlob(excludePattern, relative)
			if err != nil {
				return fmt.Errorf("invalid exclude pattern: %w", err)
			}
			if excluded {
				return nil
			}
		}
		result = append(result, relative)
		return nil
	})
	sort.Strings(result)
	return result, err
}

func literalIndices(line, query string) [][]int {
	return literalIndicesWithCase(line, query, true)
}

func literalIndicesWithCase(line, query string, caseSensitive bool) [][]int {
	if query == "" {
		return nil
	}
	searchLine, searchQuery := line, query
	if !caseSensitive {
		searchLine, searchQuery = strings.ToLower(line), strings.ToLower(query)
	}
	var result [][]int
	for offset := 0; offset <= len(searchLine); {
		index := strings.Index(searchLine[offset:], searchQuery)
		if index < 0 {
			break
		}
		start := offset + index
		result = append(result, []int{start, start + len(searchQuery)})
		offset = start + len(searchQuery)
	}
	return result
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeCursor(cursor string) int {
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	offset, _ := strconv.Atoi(string(decoded))
	if offset < 0 {
		return 0
	}
	return offset
}
