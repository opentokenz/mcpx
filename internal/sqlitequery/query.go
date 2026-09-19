package sqlitequery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"mcpx/internal/file"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const DefaultMaxRows = 1000

type Result struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated"`
}

// Query executes one readonly query against an existing SQLite database inside
// the workspace. The caller remains responsible for MCPX file-policy checks.
func Query(ctx context.Context, workspaceRoot, database, query string, maxRows, maxBytes int) (Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Result{}, fmt.Errorf("sqlite query required")
	}
	if hasStatementSeparator(query) {
		return Result{}, fmt.Errorf("sqlite runtime accepts one query without statement separators")
	}
	if maxRows <= 0 || maxRows > DefaultMaxRows {
		maxRows = DefaultMaxRows
	}

	db, conn, err := openReadonlyConnection(ctx, workspaceRoot, database)
	if err != nil {
		return Result{}, err
	}
	defer db.Close()
	defer conn.Close()

	wrapped := "SELECT * FROM (\n" + query + "\n) AS mcpx_query LIMIT ?"
	rows, err := conn.QueryContext(ctx, wrapped, maxRows+1)
	if err != nil {
		return Result{}, fmt.Errorf("query sqlite database: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return Result{}, fmt.Errorf("read sqlite columns: %w", err)
	}
	result := Result{Columns: columns, Rows: make([][]any, 0)}
	usedBytes := 0
	if encoded, marshalErr := json.Marshal(columns); marshalErr == nil {
		usedBytes = len(encoded)
	}

	for rows.Next() {
		if len(result.Rows) >= maxRows {
			result.Truncated = true
			break
		}
		row, scanErr := scanRow(rows, len(columns))
		if scanErr != nil {
			return Result{}, scanErr
		}
		if maxBytes > 0 {
			encoded, marshalErr := json.Marshal(row)
			if marshalErr != nil {
				return Result{}, fmt.Errorf("encode sqlite row: %w", marshalErr)
			}
			if usedBytes+len(encoded) > maxBytes {
				result.Truncated = true
				break
			}
			usedBytes += len(encoded)
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("iterate sqlite rows: %w", err)
	}
	result.RowCount = len(result.Rows)
	return result, nil
}

func openReadonlyConnection(ctx context.Context, workspaceRoot, database string) (*sql.DB, *sql.Conn, error) {
	database = strings.TrimSpace(database)
	if database == "" {
		return nil, nil, fmt.Errorf("database is required for sqlite runtime")
	}
	absolute, err := file.Resolve(workspaceRoot, database)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve sqlite database: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, nil, fmt.Errorf("stat sqlite database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("sqlite database must be a regular file")
	}

	uriPath := filepath.ToSlash(absolute)
	// SQLite file URIs require an absolute Windows drive path to carry a
	// leading slash: C:/db.sqlite -> file:///C:/db.sqlite. Without it,
	// modernc/sqlite parses the drive form incorrectly when URI parameters
	// such as mode=ro are present.
	if volume := filepath.VolumeName(absolute); len(volume) == 2 && volume[1] == ':' && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsnURL := url.URL{Scheme: "file", Path: uriPath}
	params := url.Values{}
	params.Set("mode", "ro")
	params.Add("_pragma", "query_only(1)")
	params.Add("_pragma", "trusted_schema(0)")
	dsnURL.RawQuery = params.Encode()

	db, err := sql.Open("sqlite", dsnURL.String())
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("connect sqlite database: %w", err)
	}
	if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_ATTACHED, 0); err != nil {
		conn.Close()
		db.Close()
		return nil, nil, fmt.Errorf("limit sqlite attachments: %w", err)
	}
	return db, conn, nil
}

func scanRow(rows *sql.Rows, columnCount int) ([]any, error) {
	values := make([]any, columnCount)
	destinations := make([]any, columnCount)
	for index := range values {
		destinations[index] = &values[index]
	}
	if err := rows.Scan(destinations...); err != nil {
		return nil, fmt.Errorf("scan sqlite row: %w", err)
	}
	for index, value := range values {
		if bytes, ok := value.([]byte); ok {
			values[index] = append([]byte(nil), bytes...)
		}
	}
	return values, nil
}

// hasStatementSeparator recognizes only the lexical constructs needed to keep
// a query inside the wrapper statement. SQLite itself remains the SQL parser.
func hasStatementSeparator(query string) bool {
	const (
		normal = iota
		singleQuoted
		doubleQuoted
		backtickQuoted
		bracketQuoted
		lineComment
		blockComment
	)
	state := normal
	for index := 0; index < len(query); index++ {
		current := query[index]
		next := byte(0)
		if index+1 < len(query) {
			next = query[index+1]
		}
		switch state {
		case normal:
			switch {
			case current == ';':
				return true
			case current == '\'':
				state = singleQuoted
			case current == '"':
				state = doubleQuoted
			case current == '`':
				state = backtickQuoted
			case current == '[':
				state = bracketQuoted
			case current == '-' && next == '-':
				state = lineComment
				index++
			case current == '/' && next == '*':
				state = blockComment
				index++
			}
		case singleQuoted:
			if current == '\'' {
				if next == '\'' {
					index++
				} else {
					state = normal
				}
			}
		case doubleQuoted:
			if current == '"' {
				if next == '"' {
					index++
				} else {
					state = normal
				}
			}
		case backtickQuoted:
			if current == '`' {
				if next == '`' {
					index++
				} else {
					state = normal
				}
			}
		case bracketQuoted:
			if current == ']' {
				state = normal
			}
		case lineComment:
			if current == '\n' || current == '\r' {
				state = normal
			}
		case blockComment:
			if current == '*' && next == '/' {
				state = normal
				index++
			}
		}
	}
	return false
}
