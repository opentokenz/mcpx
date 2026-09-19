package browseruse

import "testing"

func TestOfficialBrowserManifestRecognitionDoesNotDependOnExtensionID(t *testing.T) {
	var manifest extensionManifest
	manifest.ManifestVersion = 3
	manifest.Name = "ChatGPT"
	manifest.Version = "1.0.0"
	manifest.UpdateURL = "https://edge.microsoft.com/extensionwebstorebase/v1/crx"
	manifest.Action.DefaultTitle = "ChatGPT"
	manifest.Background.ServiceWorker = "background.js"
	manifest.SidePanel.DefaultPath = "codex-sidepanel/index.html"
	manifest.Commands = map[string]any{"open-codex-side-panel": map[string]any{}}
	manifest.Permissions = []string{"debugger", "nativeMessaging", "tabs", "scripting"}
	manifest.ContentScripts = append(manifest.ContentScripts, struct {
		Matches []string `json:"matches"`
		JS      []string `json:"js"`
	}{Matches: []string{"https://chatgpt.com/*"}})
	manifest.ContentSecurityPolicy.ExtensionPages = "connect-src 'self' https://chatgpt.com https://api.openai.com;"
	if !isOfficialBrowserManifest(manifest) {
		t.Fatal("expected ChatGPT Browser Use manifest signature to be recognized")
	}

	manifest.SidePanel.DefaultPath = "other/index.html"
	if isOfficialBrowserManifest(manifest) {
		t.Fatal("expected unrelated extension manifest to be rejected")
	}
}
