package browseruse

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const maxSidecarResponseBytes = 32 * 1024 * 1024

//go:embed browser_service_sidecar.mjs
var browserServiceSidecar []byte

type Elicitation struct {
	Fingerprint string         `json:"fingerprint"`
	Message     string         `json:"message"`
	Meta        map[string]any `json:"meta,omitempty"`
}

type ServiceError struct {
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

type ServiceRequest struct {
	SessionID            string
	TurnID               string
	BrowserInstanceID    string
	Command              map[string]any
	ApprovedFingerprints []string
}

type ServiceResponse struct {
	Result       any            `json:"result"`
	Error        *ServiceError  `json:"error,omitempty"`
	Elicitations []Elicitation  `json:"elicitations,omitempty"`
	ContentItems []any          `json:"content_items,omitempty"`
	ResponseMeta map[string]any `json:"response_meta,omitempty"`
}

type sidecarRequest struct {
	ID                   uint64         `json:"id"`
	SessionID            string         `json:"session_id"`
	TurnID               string         `json:"turn_id"`
	BrowserInstanceID    string         `json:"browser_instance_id,omitempty"`
	Command              map[string]any `json:"command"`
	ApprovedFingerprints []string       `json:"approved_fingerprints,omitempty"`
}

type sidecarResponse struct {
	ID           uint64         `json:"id"`
	OK           bool           `json:"ok"`
	Result       any            `json:"result"`
	Error        *ServiceError  `json:"error,omitempty"`
	Elicitations []Elicitation  `json:"elicitations,omitempty"`
	ContentItems []any          `json:"content_items,omitempty"`
	ResponseMeta map[string]any `json:"response_meta,omitempty"`
}

// Service keeps one official Browser Service process alive so claimed tabs and
// DOM-CUA node ids remain valid across MCP calls in the same MCPX process.
type Service struct {
	mu sync.Mutex

	nextID uint64
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	done   chan error
	stderr lockedBuffer
	tempJS string
}

func NewService() *Service { return &Service{} }

func (s *Service) Execute(ctx context.Context, request ServiceRequest) (ServiceResponse, error) {
	if s == nil {
		return ServiceResponse{}, errors.New("browser service is unavailable")
	}
	if strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.TurnID) == "" {
		return ServiceResponse{}, errors.New("browser service session_id and turn_id are required")
	}
	if request.Command == nil || strings.TrimSpace(stringValue(request.Command["type"])) == "" {
		return ServiceResponse{}, errors.New("browser service command type is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.startLocked(); err != nil {
		return ServiceResponse{}, err
	}

	s.nextID++
	wire := sidecarRequest{
		ID:                   s.nextID,
		SessionID:            request.SessionID,
		TurnID:               request.TurnID,
		BrowserInstanceID:    request.BrowserInstanceID,
		Command:              request.Command,
		ApprovedFingerprints: request.ApprovedFingerprints,
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return ServiceResponse{}, fmt.Errorf("encode browser service request: %w", err)
	}
	encoded = append(encoded, '\n')
	if _, err := s.stdin.Write(encoded); err != nil {
		detail := s.stderr.String()
		s.resetLocked(true)
		return ServiceResponse{}, fmt.Errorf("write browser service request: %w%s", err, stderrSuffix(detail))
	}

	type readResult struct {
		line []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	go func(reader *bufio.Reader) {
		line, readErr := reader.ReadBytes('\n')
		readCh <- readResult{line: line, err: readErr}
	}(s.stdout)

	var read readResult
	select {
	case read = <-readCh:
	case <-ctx.Done():
		s.resetLocked(true)
		<-readCh
		return ServiceResponse{}, ctx.Err()
	}
	if read.err != nil {
		detail := s.stderr.String()
		s.resetLocked(true)
		return ServiceResponse{}, fmt.Errorf("read browser service response: %w%s", read.err, stderrSuffix(detail))
	}
	if len(read.line) > maxSidecarResponseBytes {
		s.resetLocked(true)
		return ServiceResponse{}, fmt.Errorf("browser service response exceeds %d bytes", maxSidecarResponseBytes)
	}
	var wireResponse sidecarResponse
	if err := json.Unmarshal(bytes.TrimSpace(read.line), &wireResponse); err != nil {
		return ServiceResponse{}, fmt.Errorf("decode browser service response: %w", err)
	}
	if wireResponse.ID != wire.ID {
		s.resetLocked(true)
		return ServiceResponse{}, fmt.Errorf("browser service response id mismatch: got %d want %d", wireResponse.ID, wire.ID)
	}
	return ServiceResponse{
		Result:       wireResponse.Result,
		Error:        wireResponse.Error,
		Elicitations: wireResponse.Elicitations,
		ContentItems: wireResponse.ContentItems,
		ResponseMeta: wireResponse.ResponseMeta,
	}, nil
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetLocked(true)
}

func (s *Service) startLocked() error {
	if s.cmd != nil {
		select {
		case err := <-s.done:
			s.done = nil
			s.cmd = nil
			s.stdin = nil
			s.stdout = nil
			if s.tempJS != "" {
				_ = os.Remove(s.tempJS)
				s.tempJS = ""
			}
			if err != nil {
				return fmt.Errorf("browser service exited: %w%s", err, stderrSuffix(s.stderr.String()))
			}
		default:
			return nil
		}
	}

	host, err := FindNodeReplHost()
	if err != nil {
		return err
	}
	servicePath, err := FindBrowserService()
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp("", "mcpx-browser-service-*.mjs")
	if err != nil {
		return fmt.Errorf("create browser service sidecar: %w", err)
	}
	tempPath := temp.Name()
	if _, err := temp.Write(browserServiceSidecar); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("write browser service sidecar: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close browser service sidecar: %w", err)
	}

	cmd := exec.Command(host.NodePath, tempPath)
	cmd.Env = append(os.Environ(),
		"MCPX_BROWSER_SERVICE_PATH="+servicePath,
		"MCPX_NODE_REPL_PATH="+host.NodeReplPath,
		"MCPX_NODE_PATH="+host.NodePath,
		"MCPX_CODEX_CLI_PATH="+host.CodexCLIPath,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	s.stderr.Reset()
	cmd.Stderr = &s.stderr
	if err := cmd.Start(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("start browser service: %w", err)
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = bufio.NewReaderSize(stdoutPipe, 256*1024)
	s.done = make(chan error, 1)
	s.tempJS = tempPath
	go func(done chan<- error) { done <- cmd.Wait() }(s.done)
	return nil
}

func (s *Service) resetLocked(kill bool) error {
	var result error
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil && kill {
		_ = s.cmd.Process.Kill()
	}
	if s.done != nil {
		if err := <-s.done; err != nil && !kill {
			result = err
		}
	}
	if s.tempJS != "" {
		_ = os.Remove(s.tempJS)
	}
	s.cmd = nil
	s.stdin = nil
	s.stdout = nil
	s.done = nil
	s.tempJS = ""
	return result
}

func FindBrowserService() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("MCPX_BROWSER_SERVICE_PATH")); explicit != "" {
		if info, err := os.Stat(explicit); err == nil && !info.IsDir() {
			return explicit, nil
		}
		return "", fmt.Errorf("MCPX_BROWSER_SERVICE_PATH does not point to a file: %s", explicit)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".codex", "plugins", "cache", "openai-bundled", "chrome", "latest", "scripts", "browser-service.mjs")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	if runtime.GOOS == "windows" {
		if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
			manifest := filepath.Join(local, "OpenAI", "extension", "com.openai.codexextension.json")
			if raw, err := os.ReadFile(manifest); err == nil {
				var parsed struct {
					Path string `json:"path"`
				}
				if json.Unmarshal(raw, &parsed) == nil && strings.TrimSpace(parsed.Path) != "" {
					root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(parsed.Path))))
					candidate := filepath.Join(root, "scripts", "browser-service.mjs")
					if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
						return candidate, nil
					}
				}
			}
		}
	}
	return "", errors.New("OpenAI browser-service.mjs was not found; install or update the official ChatGPT/Codex browser integration")
}

func stderrSuffix(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) > 4096 {
		value = value[len(value)-4096:]
	}
	return ": " + value
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b.Reset()
}
