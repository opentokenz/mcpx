package browseruse

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const maxFrameBytes = 8 * 1024 * 1024

var ErrUnsupported = errors.New("OpenAI browser extension integration is only supported on Windows")

type ID string

func (id *ID) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*id = ""
		return nil
	}
	if data[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		*id = ID(value)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*id = ID(number.String())
	return nil
}

type Metadata struct {
	ExtensionID         string `json:"extensionId,omitempty"`
	ExtensionInstanceID string `json:"extensionInstanceId,omitempty"`
	CodexSessionID      string `json:"codexSessionId,omitempty"`
}

type Info struct {
	AgentRequestHeaderEnabled *bool          `json:"agentRequestHeaderEnabled,omitempty"`
	Capabilities              map[string]any `json:"capabilities,omitempty"`
	Family                    string         `json:"family,omitempty"`
	Metadata                  Metadata       `json:"metadata,omitempty"`
	Name                      string         `json:"name,omitempty"`
	Type                      string         `json:"type,omitempty"`
	Version                   string         `json:"version,omitempty"`
}

type Tab struct {
	ID            ID     `json:"id"`
	ProviderTabID string `json:"providerTabId,omitempty"`
	Title         string `json:"title,omitempty"`
	URL           string `json:"url,omitempty"`
	LastOpened    string `json:"lastOpened,omitempty"`
	TabGroup      string `json:"tabGroup,omitempty"`
}

type Backend struct {
	Pipe string
	Info Info
}

type Client struct {
	Pipe string
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error,omitempty"`
}

func InspectOfficial(ctx context.Context) (Discovery, error) {
	installations, err := DiscoverInstalledOfficial()
	if err != nil {
		return Discovery{}, err
	}
	pipes, err := browserPipeCandidates()
	if err != nil {
		return Discovery{}, err
	}
	discovery := Discovery{Installations: installations, PipeCount: len(pipes)}
	seen := map[string]struct{}{}
	for _, pipe := range pipes {
		probeCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
		client := Client{Pipe: pipe}
		info, probeErr := client.GetInfo(probeCtx)
		if probeErr == nil && info.Type == "extension" && matchesOfficialInstallation(info, installations) {
			probeErr = client.Ping(probeCtx)
		}
		cancel()
		if probeErr != nil || info.Type != "extension" || !matchesOfficialInstallation(info, installations) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(info.Family)) + "\x00" + strings.TrimSpace(info.Metadata.ExtensionID) + "\x00" + strings.TrimSpace(info.Metadata.ExtensionInstanceID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		discovery.Backends = append(discovery.Backends, Backend{Pipe: pipe, Info: info})
	}
	sort.Slice(discovery.Backends, func(i, j int) bool {
		if discovery.Backends[i].Info.Family != discovery.Backends[j].Info.Family {
			return discovery.Backends[i].Info.Family < discovery.Backends[j].Info.Family
		}
		return discovery.Backends[i].Info.Metadata.ExtensionInstanceID < discovery.Backends[j].Info.Metadata.ExtensionInstanceID
	})
	return discovery, nil
}

func DiscoverOfficial(ctx context.Context) ([]Backend, error) {
	discovery, err := InspectOfficial(ctx)
	if err != nil {
		return nil, err
	}
	return discovery.Backends, nil
}

func FilterBackends(backends []Backend, instanceID string) []Backend {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return backends
	}
	filtered := make([]Backend, 0, 1)
	for _, backend := range backends {
		if backend.Info.Metadata.ExtensionInstanceID == instanceID {
			filtered = append(filtered, backend)
		}
	}
	return filtered
}

func (c Client) Ping(ctx context.Context) error {
	var result string
	if err := c.call(ctx, "ping", map[string]any{}, &result); err != nil {
		return err
	}
	if result != "pong" {
		return fmt.Errorf("unexpected browser ping result %q", result)
	}
	return nil
}

func (c Client) GetInfo(ctx context.Context) (Info, error) {
	var result Info
	err := c.call(ctx, "getInfo", map[string]any{}, &result)
	return result, err
}

func (c Client) UserTabs(ctx context.Context) ([]Tab, error) {
	var result []Tab
	err := c.call(ctx, "getUserTabs", map[string]any{}, &result)
	return result, err
}

func (c Client) call(ctx context.Context, method string, params any, result any) error {
	if strings.TrimSpace(c.Pipe) == "" {
		return errors.New("browser pipe is required")
	}
	conn, err := openBrowserPipe(ctx, c.Pipe)
	if err != nil {
		return err
	}
	defer conn.Close()

	closed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-closed:
		}
	}()
	defer close(closed)

	request := map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": 1}
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if err := writeFrame(conn, payload); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	payload, err = readFrame(conn)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	var response rpcResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return fmt.Errorf("decode browser RPC response: %w", err)
	}
	if response.Error != nil {
		return fmt.Errorf("browser RPC %s failed (%d): %s", method, response.Error.Code, response.Error.Message)
	}
	if result == nil || len(response.Result) == 0 || string(response.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("decode browser RPC %s result: %w", method, err)
	}
	return nil
}

func writeFrame(w io.Writer, payload []byte) error {
	if len(payload) > maxFrameBytes {
		return fmt.Errorf("browser RPC frame exceeds %d bytes", maxFrameBytes)
	}
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(len(payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(header)
	if size > maxFrameBytes {
		return nil, fmt.Errorf("browser RPC frame exceeds %d bytes", maxFrameBytes)
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
