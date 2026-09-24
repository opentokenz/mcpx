package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/auth"
	"mcpx/internal/mcpresult"
	"mcpx/internal/remotesession"
	"mcpx/internal/screenshot"
)

var fakeScreenshotData = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}

type fakeScreenCapturer struct{}

func (fakeScreenCapturer) Capture(_ context.Context, request screenshot.Request) (screenshot.Result, error) {
	digest := sha256.Sum256(fakeScreenshotData)
	return screenshot.Result{
		Data: append([]byte(nil), fakeScreenshotData...),
		Metadata: screenshot.Metadata{
			Mode: request.Mode, Display: request.Display, X: request.X, Y: request.Y,
			CapturedWidth: request.Width, CapturedHeight: request.Height,
			OutputWidth: 300, OutputHeight: 200, Compression: request.Compression,
			Format: "jpeg", MIMEType: "image/jpeg", Bytes: len(fakeScreenshotData),
			SHA256: "sha256:" + hex.EncodeToString(digest[:]),
		},
	}, nil
}

func newScreenshotTestSession(t *testing.T) (*Runtime, string, string, context.Context) {
	t.Helper()
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, _ := rt.reg.Get("demo")
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: "demo", WorkspacePath: registered.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.screenshot = fakeScreenCapturer{}
	ctx := auth.ContextWithAuthorization(context.Background(), "Bearer developer-token")
	return rt, created.Session.ID, registered.Path, ctx
}

func TestScreenshotResultIncludesImageAndRecoverableArtifact(t *testing.T) {
	rt, remoteSessionID, workspaceRoot, ctx := newScreenshotTestSession(t)

	res, err := rt.toolScreenshotCapture(ctx, mcpresult.Request(map[string]any{
		"purpose": "capture screenshot", "remote_session_id": remoteSessionID, "mode": "fullscreen",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) < 2 {
		t.Fatalf("content len=%d sc=%+v", len(res.Content), res.StructuredContent)
	}
	image, ok := res.Content[1].(*mcp.ImageContent)
	if !ok || image.MIMEType != "image/jpeg" || !bytes.Equal(image.Data, fakeScreenshotData) {
		t.Fatalf("image content=%#v", res.Content[1])
	}

	data := structuredBusinessData(res)
	artifactID, _ := data["artifact_id"].(string)
	resourceURI, _ := data["resource_uri"].(string)
	if artifactID == "" || resourceURI == "" {
		t.Fatalf("screenshot structured data missing artifact reference: %+v", data)
	}

	registered, content, err := rt.artifacts.ReadAll(ctx, remoteSessionID, artifactID, workspaceRoot, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if registered.Kind != "screenshot" || registered.Path != "" || registered.ResourceURI != resourceURI {
		t.Fatalf("registered artifact=%+v", registered)
	}
	if !bytes.Equal(content, fakeScreenshotData) || registered.SHA256 != data["sha256"] || registered.Size != int64(len(fakeScreenshotData)) {
		t.Fatalf("artifact content/metadata mismatch artifact=%+v data=%+v", registered, data)
	}

	listed, err := rt.artifacts.List(ctx, remoteSessionID, "screenshot", 10)
	if err != nil || len(listed) != 1 || listed[0].ID != artifactID {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}

	readResult, err := rt.toolArtifactRead(ctx, mcpresult.Request(map[string]any{
		"remote_session_id": remoteSessionID, "artifact_id": artifactID, "limit": len(fakeScreenshotData),
	}))
	if err != nil {
		t.Fatal(err)
	}
	readData := structuredBusinessData(readResult)
	if readData["base64"] != base64.StdEncoding.EncodeToString(fakeScreenshotData) || readData["delivery_encoding"] != "base64" {
		t.Fatalf("artifact read data=%+v", readData)
	}

	resource, err := rt.resourceArtifact(ctx, &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: resourceURI}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resource.Contents) != 1 || resource.Contents[0].MIMEType != "image/jpeg" || !bytes.Equal(resource.Contents[0].Blob, fakeScreenshotData) {
		t.Fatalf("artifact resource=%+v", resource)
	}
}

func TestScreenshotFailsWhenArtifactPersistenceFails(t *testing.T) {
	rt, remoteSessionID, _, ctx := newScreenshotTestSession(t)
	if _, err := rt.state.DB().ExecContext(ctx, "DROP TABLE artifact_blobs"); err != nil {
		t.Fatal(err)
	}

	res, err := rt.toolScreenshotCapture(ctx, mcpresult.Request(map[string]any{
		"purpose": "capture screenshot", "remote_session_id": remoteSessionID, "mode": "fullscreen",
	}))
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeToolResult(t, res)
	if statusOK(decoded) {
		t.Fatalf("screenshot unexpectedly succeeded without artifact persistence: %+v", decoded)
	}
	errBody, _ := decoded["error"].(map[string]any)
	if errBody["code"] != "SCREENSHOT_ARTIFACT_ERROR" {
		t.Fatalf("error=%+v", errBody)
	}
}
