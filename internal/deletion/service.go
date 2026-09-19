// Package deletion persists frozen, workspace-scoped move-out requests.
// Filesystem validation and mutation stay in the server package; this package
// only owns the durable request state and manifest identity.
package deletion

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const DefaultTTL = 30 * time.Minute

// NewUUID creates the server-issued credential returned by move_out_prepare.
// It is deliberately generated here rather than accepted from tool input.
func NewUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate confirmation uuid: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return hex.EncodeToString(raw[0:4]) + "-" +
		hex.EncodeToString(raw[4:6]) + "-" +
		hex.EncodeToString(raw[6:8]) + "-" +
		hex.EncodeToString(raw[8:10]) + "-" +
		hex.EncodeToString(raw[10:16]), nil
}

var (
	ErrNotFound  = errors.New("move-out request not found")
	ErrConflict  = errors.New("move-out request conflict")
	ErrInProcess = errors.New("move-out request is already committing")
)

type Target struct {
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	ExpectedSHA256 string `json:"expected_sha256"`
	Revision       string `json:"rev,omitempty"`
	LinkTarget     string `json:"link_target,omitempty"`
	LinkSHA256     string `json:"link_sha256,omitempty"`
	Size           int64  `json:"size"`
}

type Manifest struct {
	Workspace       string   `json:"workspace"`
	Targets         []Target `json:"targets"`
	FileCount       int      `json:"file_count"`
	DirectoryCount  int      `json:"directory_count"`
	SymlinkCount    int      `json:"symlink_count"`
	TotalBytes      int64    `json:"total_bytes"`
	TotalBytesKnown bool     `json:"total_bytes_known"`
}

func (m Manifest) Bytes() ([]byte, error) { return json.Marshal(m) }

func (m Manifest) SHA256() (string, error) {
	b, err := m.Bytes()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// HashConfirmationUUID stores only the digest of the server-generated
// confirmation credential. The UUID itself is returned by move_out_prepare and
// must be presented by submit_move_out after the web client has asked the user.
func HashConfirmationUUID(uuid string) string {
	sum := sha256.Sum256([]byte(uuid))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type Request struct {
	ID                   string
	RemoteSessionID      string
	PrincipalID          string
	Workspace            string
	WorkspacePath        string
	Purpose              string
	IdempotencyKey       string
	Manifest             Manifest
	ManifestSHA256       string
	Status               string
	ConfirmationUUIDHash string
	ResultJSON           []byte
	CreatedAt            time.Time
	ExpiresAt            time.Time
	UpdatedAt            time.Time
	CommittedAt          *time.Time
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Create(ctx context.Context, item Request) error {
	if s == nil || s.db == nil {
		return errors.New("delete store database is required")
	}
	manifest, err := item.Manifest.Bytes()
	if err != nil {
		return fmt.Errorf("encode move-out manifest: %w", err)
	}
	now := item.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	expires := item.ExpiresAt.UTC()
	if expires.IsZero() {
		expires = now.Add(DefaultTTL)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO delete_requests
		(id, remote_session_id, principal_id, workspace_name, workspace_path, purpose,
		 idempotency_key, manifest_json, manifest_sha256, status, approval_receipt_hash,
		 result_json, created_at, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.RemoteSessionID, item.PrincipalID, item.Workspace, item.WorkspacePath,
		item.Purpose, item.IdempotencyKey, string(manifest), item.ManifestSHA256,
		firstStatus(item.Status, "prepared"), item.ConfirmationUUIDHash, jsonBytes(item.ResultJSON),
		now.UnixMilli(), expires.UnixMilli(), now.UnixMilli())
	if err != nil {
		return fmt.Errorf("persist move-out request: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (Request, error) {
	if s == nil || s.db == nil {
		return Request{}, errors.New("delete store database is required")
	}
	var item Request
	var manifestJSON, resultJSON string
	var created, expires, updated int64
	var committed sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, remote_session_id, principal_id, workspace_name,
		workspace_path, purpose, idempotency_key, manifest_json, manifest_sha256, status,
		approval_receipt_hash, result_json, created_at, expires_at, updated_at, committed_at
		FROM delete_requests WHERE id = ?`, id).Scan(
		&item.ID, &item.RemoteSessionID, &item.PrincipalID, &item.Workspace, &item.WorkspacePath,
		&item.Purpose, &item.IdempotencyKey, &manifestJSON, &item.ManifestSHA256, &item.Status,
		&item.ConfirmationUUIDHash, &resultJSON, &created, &expires, &updated, &committed)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, err
	}
	if err := json.Unmarshal([]byte(manifestJSON), &item.Manifest); err != nil {
		return Request{}, fmt.Errorf("decode move-out manifest: %w", err)
	}
	item.ResultJSON = []byte(resultJSON)
	item.CreatedAt = time.UnixMilli(created).UTC()
	item.ExpiresAt = time.UnixMilli(expires).UTC()
	item.UpdatedAt = time.UnixMilli(updated).UTC()
	if committed.Valid {
		value := time.UnixMilli(committed.Int64).UTC()
		item.CommittedAt = &value
	}
	return item, nil
}

func (s *Store) FindByIdempotency(ctx context.Context, remoteSessionID, principalID, key string) (Request, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM delete_requests
		WHERE remote_session_id = ? AND principal_id = ? AND idempotency_key = ?
		ORDER BY created_at DESC LIMIT 1`, remoteSessionID, principalID, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, err
	}
	return s.Get(ctx, id)
}

func (s *Store) MarkCommitting(ctx context.Context, id string) (Request, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE delete_requests SET status = 'committing', updated_at = ?
		WHERE id = ? AND status = 'prepared' AND expires_at > ?`, time.Now().UTC().UnixMilli(), id, time.Now().UTC().UnixMilli())
	if err != nil {
		return Request{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		item, getErr := s.Get(ctx, id)
		if getErr != nil {
			return Request{}, getErr
		}
		if item.Status == "committing" {
			return Request{}, ErrInProcess
		}
		return item, ErrConflict
	}
	return s.Get(ctx, id)
}

func (s *Store) Complete(ctx context.Context, id, status string, resultJSON []byte) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE delete_requests SET status = ?, result_json = ?, committed_at = ?, updated_at = ? WHERE id = ?`,
		status, jsonBytes(resultJSON), now.UnixMilli(), now.UnixMilli(), id)
	return err
}

func jsonBytes(value []byte) string {
	if len(value) == 0 {
		return "{}"
	}
	return string(value)
}

func firstStatus(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
