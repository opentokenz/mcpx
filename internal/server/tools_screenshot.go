package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"

	"mcpx/internal/audit"
	"mcpx/internal/envelope"
	"mcpx/internal/remotesession"
	"mcpx/internal/screenshot"
)

type screenCapturer interface {
	Capture(context.Context, screenshot.Request) (screenshot.Result, error)
}

type screenshotCaptureData struct {
	screenshot.Metadata
	ArtifactID  string `json:"artifact_id"`
	ResourceURI string `json:"resource_uri"`
}

func (r *Runtime) toolScreenshotCapture(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	request := screenshot.Request{
		Mode: stringPayload(envReq.Payload, "mode"), Compression: stringPayload(envReq.Payload, "compression"),
		Format: stringPayload(envReq.Payload, "format"), Display: intPayload(envReq.Payload, "display"),
		X: intPayload(envReq.Payload, "x"), Y: intPayload(envReq.Payload, "y"),
		Width: intPayload(envReq.Payload, "width"), Height: intPayload(envReq.Payload, "height"),
		Quality: intPayload(envReq.Payload, "quality"), MaxWidth: intPayload(envReq.Payload, "max_width"),
		MaxHeight: intPayload(envReq.Payload, "max_height"),
	}
	captured, err := r.screenshot.Capture(ctx, request)
	if err != nil {
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, nil, "screenshot_error", err.Error())
		response.RemoteSessionID = session.ID
		return r.resultJSON(response)
	}

	registered, err := r.artifacts.RegisterBytes(
		ctx,
		session.ID,
		principal.ID,
		captured.Data,
		"screenshot."+captured.Metadata.Format,
		"screenshot",
		captured.Metadata.MIMEType,
	)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "screenshot_artifact_error", err.Error())
	}
	_ = r.remote.AddEvent(ctx, principal, remotesession.Event{
		RemoteSessionID: session.ID,
		Type:            "artifact.registered",
		OperationID:     registered.ID,
		Summary:         registered.Name,
		ResourceURI:     registered.ResourceURI,
	})

	result, err := r.remoteResult(envReq, session.ID, session.WorkspaceName, screenshotCaptureData{
		Metadata:    captured.Metadata,
		ArtifactID:  registered.ID,
		ResourceURI: registered.ResourceURI,
	})
	if err != nil {
		return nil, err
	}
	// Image is host-visible content; structured data also carries a durable artifact fallback.
	result.Content = append(result.Content, mcpresult.NewImage(captured.Data, captured.Metadata.MIMEType))
	r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: session.ID, Workspace: session.WorkspaceName, Tool: "screenshot_capture", Status: "ok", Detail: map[string]any{
		"mode": captured.Metadata.Mode, "display": captured.Metadata.Display,
		"width": captured.Metadata.OutputWidth, "height": captured.Metadata.OutputHeight,
		"format": captured.Metadata.Format, "bytes": captured.Metadata.Bytes, "sha256": captured.Metadata.SHA256,
		"artifact_id": registered.ID, "resource_uri": registered.ResourceURI,
	}})
	return result, nil
}

func stringPayload(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}
