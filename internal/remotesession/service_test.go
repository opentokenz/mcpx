package remotesession

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcpx/internal/auth"
	"mcpx/internal/state"
)

func testService(t *testing.T) (*Service, *state.Store) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "mcpx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewService(store.DB()), store
}

func testPrincipal(id string) auth.Principal {
	return auth.Principal{ID: id, Kind: "test", SubjectHash: id + "-hash"}
}

func TestCreateListGetAndIdempotency(t *testing.T) {
	service, _ := testService(t)
	owner := testPrincipal("owner")
	in := CreateInput{
		WorkspaceName: "mcpx", WorkspacePath: t.TempDir(), Label: "session",
		ClientRequestID: "create-1", ClientName: "client-a", ClientVersion: "1",
	}
	first, err := service.Create(context.Background(), owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.Session.ID, "rs_") || len(first.Session.ID) != 19 {
		t.Fatalf("remote session id must be compact 96-bit base64url form: %q", first.Session.ID)
	}
	second, err := service.Create(context.Background(), owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Session.ID != second.Session.ID {
		t.Fatalf("idempotent result changed: %+v %+v", first, second)
	}
	if first.Replayed || !second.Replayed {
		t.Fatalf("idempotent replay contract violated: first=%+v second=%+v", first, second)
	}
	var cached string
	if err := service.db.QueryRow(`SELECT response_json FROM idempotency_records
		WHERE principal_id = ? AND client_request_id = ?`, owner.ID, in.ClientRequestID).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cached, `"replayed":true`) {
		t.Fatalf("idempotency record does not mark the replay: %s", cached)
	}
	list, err := service.List(context.Background(), owner, ListInput{Workspace: "mcpx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != first.Session.ID {
		t.Fatalf("list: %+v", list)
	}
	got, err := service.Get(context.Background(), owner, first.Session.ID)
	if err != nil || got.Role != "owner" {
		t.Fatalf("get: %+v err=%v", got, err)
	}
}

func TestGetRejectsNonMember(t *testing.T) {
	service, _ := testService(t)
	owner := testPrincipal("owner")
	other := testPrincipal("other")
	created, err := service.Create(context.Background(), owner, CreateInput{WorkspaceName: "mcpx", WorkspacePath: t.TempDir(), Label: "acl"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), other, created.Session.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized get: %v", err)
	}
}

func TestUpdateUsesOptimisticVersion(t *testing.T) {
	service, _ := testService(t)
	owner := testPrincipal("owner")
	created, err := service.Create(context.Background(), owner, CreateInput{WorkspaceName: "mcpx", WorkspacePath: t.TempDir(), Label: "before"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(context.Background(), owner, created.Session.ID, "after", "", "idle", created.Session.Version)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Label != "after" || updated.Status != "idle" || updated.Version != 2 {
		t.Fatalf("updated: %+v", updated)
	}
	if _, err := service.Update(context.Background(), owner, created.Session.ID, "stale", "", "", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestTouchUpdatesLastActiveAtAndThrottles(t *testing.T) {
	service, _ := testService(t)
	owner := testPrincipal("owner")
	stranger := testPrincipal("stranger")
	clock := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return clock }

	created, err := service.Create(context.Background(), owner, CreateInput{
		WorkspaceName: "mcpx", WorkspacePath: t.TempDir(), Label: "touch",
		ClientName: "client-a", ClientVersion: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Session.LastActiveAt.Equal(clock) {
		t.Fatalf("create last_active_at=%v want %v", created.Session.LastActiveAt, clock)
	}

	clock = clock.Add(2 * time.Second)
	service.Touch(context.Background(), owner, created.Session.ID, "client-a", "1")
	got, err := service.Get(context.Background(), owner, created.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastActiveAt.Equal(clock) {
		t.Fatalf("touched last_active_at=%v want %v", got.LastActiveAt, clock)
	}

	throttled := clock
	service.Touch(context.Background(), owner, created.Session.ID, "client-a", "1")
	got, err = service.Get(context.Background(), owner, created.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastActiveAt.Equal(throttled) {
		t.Fatalf("throttled last_active_at=%v want %v", got.LastActiveAt, throttled)
	}

	clock = clock.Add(time.Second)
	service.Touch(context.Background(), owner, created.Session.ID, "client-a", "1")
	got, err = service.Get(context.Background(), owner, created.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastActiveAt.Equal(clock) {
		t.Fatalf("second touch last_active_at=%v want %v", got.LastActiveAt, clock)
	}

	var memberActive, clientSeen int64
	if err := service.db.QueryRow(`SELECT last_active_at FROM remote_session_members WHERE remote_session_id = ? AND principal_id = ?`,
		created.Session.ID, owner.ID).Scan(&memberActive); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT last_seen_at FROM remote_session_clients WHERE remote_session_id = ? AND principal_id = ?`,
		created.Session.ID, owner.ID).Scan(&clientSeen); err != nil {
		t.Fatal(err)
	}
	if memberActive != clock.UnixMilli() || clientSeen != clock.UnixMilli() {
		t.Fatalf("member last_active_at=%d client last_seen_at=%d want %d", memberActive, clientSeen, clock.UnixMilli())
	}

	service.Touch(context.Background(), stranger, created.Session.ID, "client-b", "1")
	got, err = service.Get(context.Background(), owner, created.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastActiveAt.Equal(clock) {
		t.Fatalf("unauthorized touch changed last_active_at to %v", got.LastActiveAt)
	}
}

func TestSessionSurvivesStoreReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcpx.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := testPrincipal("owner")
	created, err := NewService(store.DB()).Create(context.Background(), owner, CreateInput{WorkspaceName: "mcpx", WorkspacePath: dir, Label: "persist"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := NewService(reopened.DB()).Get(context.Background(), owner, created.Session.ID)
	if err != nil || got.Label != "persist" {
		t.Fatalf("reopened: %+v err=%v", got, err)
	}
}
