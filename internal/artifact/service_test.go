package artifact

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mcpx/internal/auth"
	"mcpx/internal/remotesession"
	"mcpx/internal/state"
)

func TestRegisterListReadAndDetectExternalChange(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "report.txt"), []byte("test report\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "mcpx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	principal := auth.Principal{ID: "artifact-principal", Kind: "test", SubjectHash: "artifact-subject"}
	created, err := remotesession.NewService(store.DB()).Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: "project", WorkspacePath: workspace, Label: "artifact test",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB())
	registered, err := service.Register(context.Background(), created.Session.ID, principal.ID, workspace, "report.txt", "Test report", "test_report", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if registered.ResourceURI != ResourceURI(created.Session.ID, registered.ID) {
		t.Fatalf("resource URI=%q", registered.ResourceURI)
	}
	listed, err := service.List(context.Background(), created.Session.ID, "test_report", 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	read, err := service.Read(context.Background(), created.Session.ID, registered.ID, workspace, 0, 4)
	if err != nil || read.Text != "test" || read.DeliveryEncoding != DeliveryEncodingUTF8 || read.SourceEncoding != SourceEncodingUTF8 || read.SourceOffset != 0 || read.NextSourceOffset != 4 {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "report.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ReadAll(context.Background(), created.Session.ID, registered.ID, workspace, 8<<20); !errors.Is(err, ErrChanged) {
		t.Fatalf("ReadAll err=%v, want ErrChanged", err)
	}
}

func TestRegisterBytesListReadAndReadAll(t *testing.T) {
	workspace := t.TempDir()
	store, err := state.Open(filepath.Join(t.TempDir(), "mcpx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	principal := auth.Principal{ID: "artifact-bytes-principal", Kind: "test", SubjectHash: "artifact-bytes-subject"}
	created, err := remotesession.NewService(store.DB()).Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: "project", WorkspacePath: workspace, Label: "artifact bytes test",
	})
	if err != nil {
		t.Fatal(err)
	}

	service := NewService(store.DB())
	payload := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 1, 2, 3, 4}
	registered, err := service.RegisterBytes(context.Background(), created.Session.ID, principal.ID, payload, "capture.png", "screenshot", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if registered.Path != "" || registered.Kind != "screenshot" || registered.MIMEType != "image/png" || registered.Size != int64(len(payload)) {
		t.Fatalf("registered=%+v", registered)
	}
	if registered.ResourceURI != ResourceURI(created.Session.ID, registered.ID) {
		t.Fatalf("resource URI=%q", registered.ResourceURI)
	}

	listed, err := service.List(context.Background(), created.Session.ID, "screenshot", 10)
	if err != nil || len(listed) != 1 || listed[0].ID != registered.ID || listed[0].Path != "" {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}

	read, err := service.Read(context.Background(), created.Session.ID, registered.ID, workspace, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if read.DeliveryEncoding != DeliveryEncodingBase64 || read.Base64 != base64.StdEncoding.EncodeToString(payload[:3]) || read.NextSourceOffset != 3 || read.EOF {
		t.Fatalf("read=%+v", read)
	}

	got, content, err := service.ReadAll(context.Background(), created.Session.ID, registered.ID, workspace, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != registered.ID || string(content) != string(payload) {
		t.Fatalf("artifact=%+v content=%v", got, content)
	}
}
