package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"time"

	"mcpx/internal/file"
)

var (
	ErrNotFound = errors.New("artifact not found")
	ErrChanged  = errors.New("artifact content changed after registration")
)

type Service struct {
	db  *sql.DB
	now func() time.Time
}

type Artifact struct {
	ID              string    `json:"artifact_id"`
	RemoteSessionID string    `json:"remote_session_id"`
	Name            string    `json:"name"`
	Kind            string    `json:"kind"`
	Path            string    `json:"path"`
	MIMEType        string    `json:"mime_type"`
	SourceEncoding  string    `json:"source_encoding"`
	SourceBOM       string    `json:"source_bom"`
	Size            int64     `json:"size"`
	SHA256          string    `json:"sha256"`
	ResourceURI     string    `json:"resource_uri"`
	CreatedAt       time.Time `json:"created_at"`
}

type ReadResult struct {
	Artifact         Artifact `json:"artifact"`
	SourceEncoding   string   `json:"source_encoding"`
	SourceBOM        string   `json:"source_bom"`
	DeliveryEncoding string   `json:"delivery_encoding"`
	MIMEType         string   `json:"mime_type"`
	SourceOffset     int64    `json:"source_offset"`
	NextSourceOffset int64    `json:"next_source_offset"`
	EOF              bool     `json:"eof"`
	SHA256           string   `json:"sha256"`
	Text             string   `json:"text,omitempty"`
	Base64           string   `json:"base64,omitempty"`
}

func NewService(db *sql.DB) *Service { return &Service{db: db, now: time.Now} }

func (s *Service) Register(ctx context.Context, remoteSessionID, principalID, workspaceRoot, relativePath, name, kind, mimeType string) (Artifact, error) {
	absolute, err := file.Resolve(workspaceRoot, relativePath)
	if err != nil {
		return Artifact{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return Artifact{}, fmt.Errorf("artifact path must be a regular file")
	}
	relativePath = filepath.ToSlash(filepath.Clean(relativePath))
	if name == "" {
		name = filepath.Base(relativePath)
	}
	if kind == "" {
		kind = "other"
	}
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(relativePath))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	}
	digest, err := fileDigest(absolute)
	if err != nil {
		return Artifact{}, err
	}
	probe, err := readProbe(absolute, 64<<10)
	if err != nil {
		return Artifact{}, err
	}
	source := DetectSourceEncoding(relativePath, probe, mimeType)
	id := randomID()
	now := s.now().UTC()
	artifact := Artifact{ID: id, RemoteSessionID: remoteSessionID, Name: name, Kind: kind, Path: relativePath,
		MIMEType: mimeType, SourceEncoding: source.Encoding, SourceBOM: source.BOM, Size: info.Size(), SHA256: digest,
		ResourceURI: ResourceURI(remoteSessionID, id), CreatedAt: now}
	_, err = s.db.ExecContext(ctx, `INSERT INTO artifacts
        (id, remote_session_id, name, kind, path, mime_type, source_encoding, source_bom, size, sha256, created_by, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, artifact.ID, artifact.RemoteSessionID, artifact.Name,
		artifact.Kind, artifact.Path, artifact.MIMEType, artifact.SourceEncoding, artifact.SourceBOM, artifact.Size, artifact.SHA256, principalID, now.UnixMilli())
	if err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func (s *Service) Get(ctx context.Context, remoteSessionID, artifactID string) (Artifact, error) {
	var artifact Artifact
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `SELECT id, remote_session_id, name, kind, path, mime_type, source_encoding, source_bom, size, sha256, created_at
        FROM artifacts WHERE id = ? AND remote_session_id = ?`, artifactID, remoteSessionID).Scan(
		&artifact.ID, &artifact.RemoteSessionID, &artifact.Name, &artifact.Kind, &artifact.Path,
		&artifact.MIMEType, &artifact.SourceEncoding, &artifact.SourceBOM, &artifact.Size, &artifact.SHA256, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	artifact.CreatedAt = time.UnixMilli(createdAt).UTC()
	artifact.ResourceURI = ResourceURI(artifact.RemoteSessionID, artifact.ID)
	return artifact, nil
}

func (s *Service) List(ctx context.Context, remoteSessionID, kind string, limit int) ([]Artifact, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT id, name, kind, path, mime_type, source_encoding, source_bom, size, sha256, created_at
        FROM artifacts WHERE remote_session_id = ?`
	args := []any{remoteSessionID}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Artifact, 0, limit)
	for rows.Next() {
		var artifact Artifact
		var createdAt int64
		if err := rows.Scan(&artifact.ID, &artifact.Name, &artifact.Kind, &artifact.Path,
			&artifact.MIMEType, &artifact.SourceEncoding, &artifact.SourceBOM, &artifact.Size, &artifact.SHA256, &createdAt); err != nil {
			return nil, err
		}
		artifact.RemoteSessionID = remoteSessionID
		artifact.CreatedAt = time.UnixMilli(createdAt).UTC()
		artifact.ResourceURI = ResourceURI(remoteSessionID, artifact.ID)
		result = append(result, artifact)
	}
	return result, rows.Err()
}

func (s *Service) Count(ctx context.Context, remoteSessionID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE remote_session_id = ?`, remoteSessionID).Scan(&count)
	return count, err
}

func (s *Service) Read(ctx context.Context, remoteSessionID, artifactID, workspaceRoot string, offset int64, limit int) (ReadResult, error) {
	artifact, err := s.Get(ctx, remoteSessionID, artifactID)
	if err != nil {
		return ReadResult{}, err
	}
	absolute, err := file.Resolve(workspaceRoot, artifact.Path)
	if err != nil {
		return ReadResult{}, err
	}
	currentDigest, err := fileDigest(absolute)
	if err != nil {
		return ReadResult{}, err
	}
	if currentDigest != artifact.SHA256 {
		return ReadResult{}, ErrChanged
	}
	if limit <= 0 || limit > 1<<20 {
		limit = 256 << 10
	}
	source := SourceEncoding{Encoding: artifact.SourceEncoding, BOM: artifact.SourceBOM}
	start, end := AlignSourceWindow(offset, limit, artifact.Size, source)
	buffer := make([]byte, end-start)
	if len(buffer) > 0 {
		handle, err := os.Open(absolute)
		if err != nil {
			return ReadResult{}, err
		}
		defer handle.Close()
		read, readErr := handle.ReadAt(buffer, start)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return ReadResult{}, readErr
		}
		buffer = buffer[:read]
		end = start + int64(read)
	}
	result := ReadResult{
		Artifact: artifact, SourceEncoding: artifact.SourceEncoding, SourceBOM: artifact.SourceBOM,
		MIMEType: stripCharset(artifact.MIMEType), SourceOffset: start, NextSourceOffset: end,
		EOF: end >= artifact.Size, SHA256: artifact.SHA256,
	}
	if decoded, ok := DecodeSourceWindow(buffer, start, source); ok {
		result.DeliveryEncoding = DeliveryEncodingUTF8
		base := stripCharset(artifact.MIMEType)
		if !isTextMIME(base) {
			base = "text/plain"
		}
		result.MIMEType = withCharset(base, "utf-8")
		result.Text = string(decoded)
		return result, nil
	}
	result.DeliveryEncoding = DeliveryEncodingBase64
	result.Base64 = base64.StdEncoding.EncodeToString(buffer)
	return result, nil
}

func (s *Service) ReadAll(ctx context.Context, remoteSessionID, artifactID, workspaceRoot string, maxBytes int64) (Artifact, []byte, error) {
	artifact, err := s.Get(ctx, remoteSessionID, artifactID)
	if err != nil {
		return Artifact{}, nil, err
	}
	if artifact.Size > maxBytes {
		return Artifact{}, nil, fmt.Errorf("artifact exceeds resource limit; use artifact_read")
	}
	absolute, err := file.Resolve(workspaceRoot, artifact.Path)
	if err != nil {
		return Artifact{}, nil, err
	}
	content, err := os.ReadFile(absolute)
	if err != nil {
		return Artifact{}, nil, err
	}
	digest := sha256.Sum256(content)
	if "sha256:"+hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return Artifact{}, nil, ErrChanged
	}
	return artifact, content, nil
}

func ResourceURI(remoteSessionID, artifactID string) string {
	return fmt.Sprintf("mcpx://remote-sessions/%s/artifacts/%s", remoteSessionID, artifactID)
}

func readProbe(path string, limit int) ([]byte, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	if limit <= 0 {
		limit = 64 << 10
	}
	buffer := make([]byte, limit)
	n, err := handle.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buffer[:n], nil
}

func fileDigest(path string) (string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func randomID() string {
	var value [12]byte
	_, _ = rand.Read(value[:])
	return "art_" + hex.EncodeToString(value[:])
}
