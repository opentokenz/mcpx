package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"

	"mcpx/internal/approval"
	"mcpx/internal/artifact"
	"mcpx/internal/audit"
	"mcpx/internal/auth"
	"mcpx/internal/config"
	"mcpx/internal/deletion"
	"mcpx/internal/envelope"
	"mcpx/internal/environment"
	"mcpx/internal/filesnapshot"
	"mcpx/internal/idempotency"
	"mcpx/internal/logging"
	"mcpx/internal/oauth"
	"mcpx/internal/observation"
	"mcpx/internal/operation"
	"mcpx/internal/plan"
	"mcpx/internal/remotesession"
	"mcpx/internal/screenshot"
	"mcpx/internal/secrets"
	"mcpx/internal/skill"
	"mcpx/internal/state"
	"mcpx/internal/terminal"
	buildversion "mcpx/internal/version"
	"mcpx/internal/workspace"
	"mcpx/internal/workspacechanges"
)

// Options configures mcpx runtime startup.
type Options struct {
	AddrOverride string
	Version      string
	Commit       string
	Date         string
}

// Runtime is the MCPX process root.
type Runtime struct {
	opts            Options
	cfg             config.Config
	reg             *workspace.Registry
	approvals       *approval.Store
	audit           *audit.Logger
	globalCfgPath   string
	tasks           *terminal.TaskManager
	secrets         *secrets.Store
	oauth           *oauth.Server
	state           *state.Store
	remote          *remotesession.Service
	environment     *environment.Service
	workspaceDiff   *workspacechanges.Service
	fileSnapshots   *filesnapshot.Store
	artifacts       *artifact.Service
	plans           *plan.Service
	deletions       *deletion.Store
	retention       *state.RetentionService
	retentionCancel context.CancelFunc
	retentionDone   chan struct{}
	screenshot      screenCapturer
	observation     *observationBridge
	operations      *operation.Service
	observerSocket  *observation.SocketServer
	activityMu      sync.Mutex
	closeOnce       sync.Once
	closeErr        error

	// For schema revision and capability catalog.
	toolIndex    map[string]mcp.Tool
	toolIndexMu  sync.RWMutex
	toolHandlers map[string]mcp.ToolHandler
	toolMeta     map[string]toolAnnotation
	// idempotency is shared by clean-core mutating tools and persists replay
	// records in the Runtime state database.
	idempotency     *idempotency.Store
	discoveryMu     sync.Mutex
	discoveries     map[string]discoveryLease
	projectConfigMu sync.RWMutex
	projectConfigs  map[string]projectConfigCacheEntry
	build           BuildInfo
}

type projectConfigCacheEntry struct {
	exists  bool
	modTime int64
	size    int64
	config  config.Config
	err     error
}

type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// New constructs a Runtime from the registered global configuration.
func New(opts Options) (*Runtime, error) {
	// First boot: create ~/.mcpx/config.yaml, empty .mcp.json, logs/, skills/
	ensured, err := config.EnsureGlobalLayout()
	if err != nil {
		return nil, fmt.Errorf("ensure global layout: %w", err)
	}
	if ensured.CreatedHome || ensured.CreatedConfig || ensured.CreatedMCP || ensured.CreatedSkills {
		log := logging.With("component", "bootstrap")
		log.Info("global home", "path", ensured.HomeDir)
		if ensured.CreatedConfig {
			log.Info("created", "file", ensured.ConfigPath)
		}
		if ensured.CreatedMCP {
			log.Info("created", "file", ensured.MCPPath)
		}
		if ensured.CreatedSkills {
			log.Info("created", "dir", ensured.SkillsDir)
		}
		if ensured.CreatedLogs {
			log.Info("created", "dir", ensured.LogDir)
		}
	}

	globalPath, err := config.GlobalConfigPath()
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadGlobal(globalPath)
	if err != nil {
		return nil, err
	}
	reg, err := workspace.NewRegistry(cfg.Workspaces)
	if err != nil {
		return nil, err
	}
	home, err := config.HomeDir()
	if err != nil {
		return nil, err
	}
	logDir := config.ExpandHome(cfg.Logging.Dir)
	if logDir == "" || strings.HasPrefix(strings.ReplaceAll(cfg.Logging.Dir, "\\", "/"), "~/.mcpx") {
		logDir = filepath.Join(home, "logs")
	}
	var logger *audit.Logger
	if cfg.Logging.Enabled {
		logger, err = audit.New(logDir)
		if err != nil {
			return nil, err
		}
	}
	if err := config.ValidateAuthMode(cfg.Auth); err != nil {
		return nil, err
	}
	if err := config.ValidateSecurityRules(cfg.Security); err != nil {
		return nil, err
	}

	oauthSrv, err := buildOAuthServer(&cfg)
	if err != nil {
		return nil, err
	}
	logStartupCredentials(cfg, oauthSrv != nil, firstNonEmpty(opts.Version, buildversion.Current))
	stateStore, err := state.Open(filepath.Join(home, "state", "mcpx.db"))
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}
	environmentService, err := environment.NewService(context.Background(), stateStore.DB())
	if err != nil {
		_ = stateStore.Close()
		return nil, fmt.Errorf("initialize environment service: %w", err)
	}
	taskLogDir := filepath.Join(home, "tasks")
	taskManager, err := terminal.NewPersistentTaskManager(stateStore.DB(), taskLogDir)
	if err != nil {
		_ = stateStore.Close()
		return nil, fmt.Errorf("initialize terminal tasks: %w", err)
	}
	retentionService, err := state.NewRetentionService(stateStore.DB(), taskLogDir, cfg.State.Retention)
	if err != nil {
		_ = stateStore.Close()
		return nil, fmt.Errorf("initialize state retention: %w", err)
	}

	runtime := &Runtime{
		opts:           opts,
		cfg:            cfg,
		reg:            reg,
		approvals:      approval.NewPersistentStore(stateStore.DB()),
		audit:          logger,
		globalCfgPath:  globalPath,
		tasks:          taskManager,
		secrets:        secrets.NewPersistentStore(stateStore.DB()),
		oauth:          oauthSrv,
		state:          stateStore,
		remote:         remotesession.NewService(stateStore.DB()),
		environment:    environmentService,
		workspaceDiff:  workspacechanges.NewService(stateStore.DB()),
		fileSnapshots:  filesnapshot.NewStore(stateStore.DB()),
		artifacts:      artifact.NewService(stateStore.DB()),
		plans:          plan.NewService(stateStore.DB()),
		deletions:      deletion.NewStore(stateStore.DB()),
		retention:      retentionService,
		screenshot:     screenshot.NewService(),
		toolIndex:      map[string]mcp.Tool{},
		toolHandlers:   map[string]mcp.ToolHandler{},
		toolMeta:       map[string]toolAnnotation{},
		idempotency:    idempotency.NewStore(stateStore.DB()),
		discoveries:    map[string]discoveryLease{},
		projectConfigs: map[string]projectConfigCacheEntry{},
		build: BuildInfo{
			Version: firstNonEmpty(opts.Version, buildversion.Current),
			Commit:  firstNonEmpty(opts.Commit, "none"),
			Date:    firstNonEmpty(opts.Date, "unknown"),
		},
	}
	obsStore := observation.NewStore(stateStore.DB())
	obsBroker := observation.NewBroker()
	bridge := &observationBridge{
		store:   obsStore,
		broker:  obsBroker,
		resolve: runtime.observationTarget,
	}
	bridge.async = observation.NewAsyncRecorder(0, bridge.Record)
	runtime.observation = bridge
	operations, err := operation.New(stateStore.DB(), operation.DefaultWorkers, runtime.observeOperationEvent)
	if err != nil {
		_ = stateStore.Close()
		return nil, fmt.Errorf("initialize operations: %w", err)
	}
	runtime.operations = operations
	taskManager.SetOutputSink(runtime.observeTaskOutput)
	runtime.observerSocket = observation.NewSocketServer(
		observation.SocketPath(home), runtime.observation.store, runtime.observation.broker,
		func(name string) bool {
			_, ok := reg.Get(strings.TrimSpace(name))
			return ok
		},
	)
	runtime.remote.SetEventObserver(runtime.observeRemoteEvent)
	// Build the catalog snapshot once at construction time so direct service
	// calls and the real MCP server observe the same registered schema.
	catalog := mcp.NewServer(&mcp.Implementation{Name: "mcpx", Version: runtime.build.Version}, nil)
	runtime.registerTools(catalog)
	return runtime, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func logStartupCredentials(cfg config.Config, oauthEnabled bool, version string) {
	fields := []any{
		"version", firstNonEmpty(version, buildversion.Current),
		"mode", config.EffectiveAuthMode(cfg.Auth),
		"token_configured", strings.TrimSpace(cfg.Auth.Token) != "",
		"oauth_password_configured", oauthEnabled && strings.TrimSpace(cfg.Auth.OAuth.Password) != "",
	}
	logging.With("component", "auth").Info("startup credentials", fields...)
}

func buildOAuthServer(cfg *config.Config) (*oauth.Server, error) {
	mode := config.EffectiveAuthMode(cfg.Auth)
	needOAuth := mode == "oauth" || mode == "dual"
	// Also enable AS when password/server_url set so metadata works for dual dogfood.
	if !needOAuth && cfg.Auth.OAuth.Password == "" && cfg.Auth.OAuth.ServerURL == "" {
		return nil, nil
	}
	password := strings.TrimSpace(cfg.Auth.OAuth.Password)
	if password == "" {
		password = oauth.TokenURLSafe(24)
		cfg.Auth.OAuth.Password = password
	}
	home, err := config.HomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve MCPX home for OAuth persistence: %w", err)
	}
	var secret []byte
	if hexKey := strings.TrimSpace(cfg.Auth.OAuth.TokenSecret); hexKey != "" {
		b, err := hex.DecodeString(hexKey)
		if err != nil {
			return nil, fmt.Errorf("auth.oauth.token_secret must be hex: %w", err)
		}
		if len(b) < 32 {
			return nil, fmt.Errorf("auth.oauth.token_secret must be at least 32 bytes")
		}
		secret = b
	} else {
		secret, err = oauth.LoadOrCreateTokenSecret(filepath.Join(home, "oauth-token-secret"))
		if err != nil {
			return nil, fmt.Errorf("persist OAuth token secret: %w", err)
		}
	}
	srv := oauth.NewServer(password, strings.TrimSpace(cfg.Auth.OAuth.ServerURL), secret, config.OAuthTokenTTL(cfg.Auth.OAuth))
	// Persist DCR clients so restart does not break ChatGPT "reconnect".
	persist := filepath.Join(home, "oauth-clients.json")
	if err := srv.Registry.SetPersistPath(persist); err != nil {
		return nil, fmt.Errorf("persist OAuth clients at %s: %w", persist, err)
	}
	logging.With("component", "oauth").Info("oauth clients store", "path", persist)
	if cid := strings.TrimSpace(cfg.Auth.OAuth.ClientID); cid != "" {
		uris := cfg.Auth.OAuth.RedirectURIs
		if len(uris) == 0 {
			uris = []string{"http://127.0.0.1/callback"}
		}
		if err := srv.Registry.AddPreregistered(cid, uris, cfg.Auth.OAuth.ClientSecret); err != nil {
			return nil, fmt.Errorf("oauth preregistered client: %w", err)
		}
	}
	return srv, nil
}

// Start serves MCP over Streamable HTTP behind the auth/OAuth gateway.
func (r *Runtime) Start() error {
	defer r.Close()
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "mcpx",
		Version: firstNonEmpty(r.build.Version, buildversion.Current),
	}, &mcp.ServerOptions{
		Instructions: agentGuidanceInstructions(),
	})
	r.registerTools(s)
	if r.observerSocket != nil {
		if err := r.observerSocket.Start(); err != nil {
			logging.With("component", "workspace_readr").Error("start socket failed", "err", err)
			return fmt.Errorf("start workspace observer: %w", err)
		}
	}
	r.startRetention()
	// toolIndex is filled by addTool during registerTools.

	addr := r.opts.AddrOverride
	if addr == "" {
		addr = r.cfg.Addr()
	}

	// DNS-rebinding protection: rejects non-loopback Host when TCP is on loopback.
	// Public IP / reverse-proxy access needs this disabled (see server.disable_localhost_protection).
	disableHostGuard := r.cfg.Server.DisableLocalhostProtection || shouldAutoDisableHostGuard(addr, r.cfg.Server.Host)
	// Stateless is required for MCP 2026-07-28 (SEP-2575): go-sdk only advertises
	// that protocol version when Stateless=true. ChatGPT Connector discovers via
	// server/discover and rejects servers that return inconsistent/unsupported
	// version sets (MCP_ACTION_DISCOVERY_FAILED / "discover response was inconsistent").
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return s
	}, &mcp.StreamableHTTPOptions{
		DisableLocalhostProtection: disableHostGuard,
		SessionTimeout:             config.TransportSessionIdleTTL(r.cfg.Transport),
		Stateless:                  true,
	})
	gw := NewGateway(r.cfg, r.oauth, streamable)

	log := logging.With("component", "server")
	log.Info("listening", "addr", addr, "transport", "streamable-http")
	public := strings.TrimSpace(r.cfg.Auth.OAuth.ServerURL)
	if public == "" {
		public = fmt.Sprintf("http://%s", addr)
	}
	log.Info("endpoint", "url", strings.TrimRight(public, "/")+"/mcp")
	log.Info("config", "path", r.globalCfgPath)
	if mcpPath, err := config.GlobalMCPPath(); err == nil {
		log.Info("mcp_json", "path", mcpPath)
	}
	if disableHostGuard {
		log.Info("host_guard", "localhost_protection", false, "hint", "non-loopback Host headers allowed; use auth on public networks")
	} else {
		log.Info("host_guard", "localhost_protection", true)
	}
	mode := config.EffectiveAuthMode(r.cfg.Auth)
	log.Info("auth", "mode", mode, "oauth", r.oauth != nil)
	if r.oauth != nil && r.cfg.Auth.OAuth.ServerURL != "" {
		log.Info("oauth", "server_url", r.cfg.Auth.OAuth.ServerURL, "resource", r.oauth.ResourceURL(r.oauth.EffectiveIssuer("")))
	}
	if mode == "open" && !isLoopbackAddr(addr) {
		log.Info("auth_warning", "msg", "auth.mode=open on non-loopback bind; not recommended for public exposure")
	}
	if r.audit != nil {
		log.Info("audit", "path", r.audit.Path())
	}
	r.logStartupInventory(log)

	srv := &http.Server{
		Addr:              addr,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

// Close releases durable process resources. It is safe to call more than once.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.stopRetention()
		if r.observation != nil && r.observation.async != nil {
			r.observation.async.Close(2 * time.Second)
		}
		if r.observerSocket != nil {
			if err := r.observerSocket.Close(); err != nil {
				r.closeErr = err
			}
		}
		if r.operations != nil {
			if err := r.operations.Close(); err != nil && r.closeErr == nil {
				r.closeErr = err
			}
		}
		if r.observation != nil && r.observation.broker != nil {
			r.observation.broker.Close()
		}
		if r.tasks != nil {
			r.tasks.Close()
		}
		if r.state != nil {
			if err := r.state.Close(); r.closeErr == nil {
				r.closeErr = err
			}
		}
	})
	return r.closeErr
}

func (r *Runtime) startRetention() {
	if r == nil || r.retention == nil || !r.cfg.State.Retention.Enabled || r.retentionDone != nil {
		return
	}
	interval, _, _, _, _, err := r.cfg.State.Retention.RetentionDurations()
	if err != nil {
		logging.With("component", "state_retention").Error("invalid retention interval", "err", err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.retentionCancel = cancel
	r.retentionDone = done
	go func() {
		defer close(done)
		// Delay the first pass so ChatGPT OAuth/discover/tools are not contending
		// with a long retention DELETE on the single SQLite connection at boot.
		startupDelay := interval
		if startupDelay > 2*time.Minute {
			startupDelay = 2 * time.Minute
		}
		if startupDelay < 30*time.Second {
			startupDelay = 30 * time.Second
		}
		timer := time.NewTimer(startupDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.runRetention(ctx)
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.runRetention(ctx)
			}
		}
	}()
}

func (r *Runtime) stopRetention() {
	if r == nil || r.retentionCancel == nil {
		return
	}
	r.retentionCancel()
	if r.retentionDone != nil {
		<-r.retentionDone
	}
	r.retentionCancel = nil
	r.retentionDone = nil
}

func (r *Runtime) runRetention(ctx context.Context) {
	if r == nil || r.retention == nil {
		return
	}
	report, err := r.retention.RunOnce(ctx)
	if err != nil {
		logging.With("component", "state_retention").Error("state cleanup failed", "err", err)
		r.recordRetentionNotice("state cleanup failed: " + err.Error())
		return
	}
	for _, message := range report.Errors {
		logging.With("component", "state_retention").Error("state maintenance warning", "err", message)
		r.recordRetentionNotice("state maintenance warning: " + message)
	}
	if report.Disabled || report.TotalDeleted() == 0 {
		return
	}
	logging.With("component", "state_retention").Info("state cleanup completed",
		"observation_events", report.DeletedObservationEvents,
		"terminal_tasks", report.DeletedTerminalTasks,
		"file_snapshots", report.DeletedFileSnapshots,
		"environment_snapshots", report.DeletedEnvironmentSnaps,
		"ephemeral_records", report.DeletedEphemeralRecords,
		"operations", report.DeletedOperations,
		"vacuumed", report.Vacuumed,
	)
}

func (r *Runtime) recordRetentionNotice(summary string) {
	if r == nil || r.observation == nil {
		return
	}
	_ = r.observation.Record(context.Background(), observation.Event{
		Workspace: "runtime",
		Type:      observation.TypeObserverNotice,
		Summary:   summary,
		CreatedAt: time.Now().UTC(),
	})
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// logStartupInventory prints workspaces, skills, and MCP servers at boot.
func (r *Runtime) logStartupInventory(log interface {
	Debug(string, ...any)
	Info(string, ...any)
}) {
	workspaces := r.reg.List()
	log.Info("──────── inventory ────────")
	log.Info("workspaces", "count", len(workspaces))
	if len(workspaces) == 0 {
		log.Info("workspace", "hint", "none registered; use workspace register <path> or edit config workspaces[]")
	}
	for i, ws := range workspaces {
		desc := ws.Description
		if proj, err := config.LoadProject(ws.Path); err == nil && proj.Description != "" {
			desc = proj.Description
		}
		log.Info("workspace",
			"index", i+1,
			"name", ws.Name,
			"path", ws.Path,
			"description", desc,
		)
	}

	// Show which skill roots we will scan (helps debug empty inventory).
	dirs := r.cfg.Discovery.Skills.Dirs
	if len(dirs) == 0 {
		dirs = skill.DefaultScanDirs()
		log.Debug("skills_dirs", "hint", "using built-in defaults; config had empty dirs")
	}
	for _, d := range dirs {
		expanded := config.ExpandHome(d)
		st := "missing"
		if fi, err := os.Stat(expanded); err == nil {
			if fi.IsDir() {
				st = "ok"
			} else {
				st = "not_dir"
			}
		}
		if d == ".skills" {
			st = "workspace-relative"
		}
		log.Debug("skills_scan", "dir", d, "expanded", expanded, "status", st)
	}

	skillSeen := map[string]bool{}
	var skillNames []string
	addSkill := func(name, runtime, format, source, scope, wsName string) {
		// A global skill directory is also part of every workspace's effective
		// configuration. Count each skill name once in the process-wide startup
		// inventory instead of once per workspace.
		if skillSeen[name] {
			return
		}
		skillSeen[name] = true
		skillNames = append(skillNames, name)
		args := []any{"name", name, "runtime", runtime, "format", format, "source", source, "scope", scope}
		if wsName != "" {
			args = append(args, "workspace", wsName)
		}
		log.Debug("skill", args...)
	}
	for _, s := range skill.LoadAll(dirs, "") {
		addSkill(s.Manifest.Name, s.Manifest.Runtime, s.Manifest.Format, s.Source, "global", "")
	}
	for _, ws := range workspaces {
		eff := r.effectiveConfig(ws.Path)
		wsDirs := eff.Discovery.Skills.Dirs
		if len(wsDirs) == 0 {
			wsDirs = dirs
		}
		for _, s := range skill.LoadAll(wsDirs, ws.Path) {
			addSkill(s.Manifest.Name, s.Manifest.Runtime, s.Manifest.Format, s.Source, "workspace", ws.Name)
		}
	}
	log.Info("skills_summary", "count", len(skillNames))
	if len(skillNames) == 0 {
		log.Info("skills", "hint", "need <dir>/<name>/SKILL.md or skill.yaml under scan dirs (e.g. ~/.agents/skills)")
	}

	mcpSeen := map[string]bool{}
	var mcpNames []string
	addMCP := func(name, typ, command, scope, wsName, source string) {
		key := scope + ":" + wsName + ":" + source + ":" + name
		if mcpSeen[key] {
			return
		}
		mcpSeen[key] = true
		mcpNames = append(mcpNames, name)
		args := []any{"name", name, "type", typ, "command", command, "scope", scope, "source", source}
		if wsName != "" {
			args = append(args, "workspace", wsName)
		}
		log.Info("mcp", args...)
	}

	if !r.cfg.Discovery.MCP.Enabled {
		log.Info("mcp", "enabled", false, "hint", "set discovery.mcp.enabled: true")
	}
	gPath, _ := config.GlobalMCPPath()
	gMCP, _ := config.LoadMCPFile(gPath)
	for name, srv := range gMCP.MCPServers {
		addMCP(name, srv.Type, srv.Command, "global", "", gPath)
	}
	for _, ws := range workspaces {
		for _, pPath := range config.ProjectMCPConfigPaths(ws.Path) {
			pMCP, _ := config.LoadMCPFile(pPath)
			for name, srv := range pMCP.MCPServers {
				addMCP(name, srv.Type, srv.Command, "workspace", ws.Name, pPath)
			}
		}
	}
	log.Info("mcp_summary", "count", len(uniqueStrings(mcpNames)), "names", strings.Join(uniqueStrings(mcpNames), ","))
	log.Info("──────── ready ────────")
}

// shouldAutoDisableHostGuard enables remote Host headers when binding all interfaces
// or an explicit non-loopback host (common VPS listen address).
func shouldAutoDisableHostGuard(listenAddr, cfgHost string) bool {
	host := cfgHost
	if host == "" {
		if h, _, err := net.SplitHostPort(listenAddr); err == nil {
			host = h
		} else {
			host = listenAddr
		}
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "0.0.0.0", "::", "":
		return true
	case "127.0.0.1", "localhost", "::1":
		return false
	default:
		// Explicit public/private NIC bind → allow that Host
		return true
	}
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func (r *Runtime) parseEnv(ctx context.Context, req *mcp.CallToolRequest) (envelope.Request, error) {
	args := mcpresult.Arguments(req)
	raw, err := json.Marshal(args)
	if err != nil {
		return envelope.Request{}, err
	}
	parsed, err := envelope.ParseRequest(raw)
	if err != nil {
		return envelope.Request{}, err
	}
	if runtime, ok := runtimeContextFrom(ctx); ok {
		parsed.RequestID = runtime.RequestID
		parsed.OperationID = runtime.OperationID
		if parsed.OperationID == "" {
			parsed.OperationID = "op_" + strings.TrimPrefix(runtime.RequestID, "req_")
		}
		parsed.ParentOperationID = runtime.ParentOperationID
		parsed.StepID = runtime.StepID
		parsed.StartedAtMs = runtime.StartedAtMs
	}
	return parsed, nil
}

// resultJSON serializes an internal handler payload into the unified tool result
// contract used before ARC instrumentation:
//
//	content[0].text     — short human summary only
//	structuredContent   — machine wire {status, data, meta, error?}
//
// instrumentTool/WrapToolResult then rewrites text into a richer host display and
// exposes model-facing {status, type, data, error?} on structuredContent.
func (r *Runtime) resultJSON(resp envelope.Response) (*mcp.CallToolResult, error) {
	b, err := envelope.Marshal(resp)
	if err != nil {
		return mcpresult.NewError(err.Error()), nil
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		return mcpresult.NewError(err.Error()), nil
	}
	return mcpresult.NewStructured(wire, envelopeHumanSummary(resp)), nil
}

func envelopeHumanSummary(resp envelope.Response) string {
	// Confirmation tokens must appear early in host-visible text before ARC wrap,
	// so previews that truncate content still leave a usable retry key.
	if label, credential := confirmationCredentialFromData(resp.Data); credential != "" {
		if label == "confirmation_uuid" {
			return "confirmation_uuid: `" + credential + "` · ask the web user, then call move_out(action=submit)"
		}
		return "confirmation_token: `" + credential + "`"
	}
	if resp.Error != nil {
		if msg := strings.TrimSpace(resp.Error.Message); msg != "" {
			return msg
		}
		if code := strings.TrimSpace(resp.Error.Code); code != "" {
			return code
		}
	}
	switch resp.Status {
	case envelope.StatusOK:
		return "succeeded"
	case envelope.StatusAccepted:
		return "accepted"
	case envelope.StatusNeedConfirmation:
		return "waiting confirmation"
	case envelope.StatusInterrupted:
		return "interrupted"
	case envelope.StatusError: // also StatusDenied / Unauthorized / NeedSecret (same wire value)
		return "failed"
	default:
		if resp.Status != "" {
			return string(resp.Status)
		}
		return "completed"
	}
}

func confirmationCredentialFromData(data any) (string, string) {
	if data == nil {
		return "", ""
	}
	m, ok := data.(map[string]any)
	if !ok {
		// Typed DTOs (e.g. commandConfirmationData) are common in Fail payloads.
		encoded, err := json.Marshal(data)
		if err != nil {
			return "", ""
		}
		if err := json.Unmarshal(encoded, &m); err != nil || m == nil {
			return "", ""
		}
	}
	if uuid, _ := m["confirmation_uuid"].(string); strings.TrimSpace(uuid) != "" {
		return "confirmation_uuid", strings.TrimSpace(uuid)
	}
	if token, _ := m["confirmation_token"].(string); strings.TrimSpace(token) != "" {
		return "confirmation_token", strings.TrimSpace(token)
	}
	// operation-style payloads nest tokens under steps.
	if steps, ok := m["steps"].([]any); ok {
		for _, step := range steps {
			sm, _ := step.(map[string]any)
			if sm == nil {
				continue
			}
			if token, _ := sm["confirmation_token"].(string); strings.TrimSpace(token) != "" {
				return "confirmation_token", strings.TrimSpace(token)
			}
		}
	}
	return "", ""
}

func (r *Runtime) writeAudit(event audit.Event) error {
	if r == nil || r.audit == nil {
		return nil
	}
	if err := r.audit.Log(event); err != nil {
		logging.With("component", "audit").Error("write failed",
			"tool", event.Tool,
			"request_id", event.RequestID,
			"err", err,
		)
		return err
	}
	return nil
}

func (r *Runtime) logAudit(event audit.Event) {
	_ = r.writeAudit(event)
}

func (r *Runtime) authAudience() (issuer, resource string) {
	if r.oauth != nil {
		issuer = r.oauth.EffectiveIssuer("")
		if issuer == "" && r.cfg.Auth.OAuth.ServerURL != "" {
			issuer = strings.TrimRight(r.cfg.Auth.OAuth.ServerURL, "/")
		}
		if issuer == "" {
			issuer = "http://" + r.cfg.Addr()
		}
		return issuer, r.oauth.ResourceURL(issuer)
	}
	if u := strings.TrimSpace(r.cfg.Auth.OAuth.ServerURL); u != "" {
		u = strings.TrimRight(u, "/")
		return u, u + "/mcp"
	}
	base := "http://" + r.cfg.Addr()
	return base, base + "/mcp"
}

// bearerFromCtx reads Authorization injected by WithHTTPContextFunc.
// Fallback MCPX_TEST_BEARER is only for unit tests without an HTTP stack.
func bearerFromCtx(ctx context.Context) string {
	if h := auth.AuthorizationFromContext(ctx); h != "" {
		return h
	}
	if v := os.Getenv("MCPX_TEST_BEARER"); v != "" {
		return v
	}
	return ""
}

func (r *Runtime) toolCapabilityList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return fail, nil
	}
	ws, remoteID, err := r.resolveExplicitWorkspace(ctx, principal, envReq)
	if err != nil {
		return r.remoteError(envReq, remoteID, ws.Name, err)
	}
	wsPath := ws.Path
	var session *remotesession.Session
	if remoteID != "" {
		resolved, sessionErr := r.remote.Get(ctx, principal, remoteID)
		if sessionErr != nil {
			return r.remoteError(envReq, remoteID, ws.Name, sessionErr)
		}
		session = &resolved
	}
	effective := r.effectiveConfig(wsPath)
	servers := []map[string]any{}
	if manager, managerErr := r.mcpManagerForWorkspace(wsPath); managerErr == nil {
		servers = manager.List()
	}
	loadedSkills := []skill.Skill{}
	if effective.Discovery.Skills.Enabled {
		loadedSkills = skill.LoadAll(effective.Discovery.Skills.Dirs, wsPath)
	}
	fullSkills := skillItems(loadedSkills)
	skills := skillSummaryItems(loadedSkills)
	tools := r.runtimeToolCapabilities(effective, session)
	fullToolManifest := r.registeredToolManifest()

	guidance := agentGuidance()
	clientProtocol := clientProtocolCapabilities()
	toolSchemaRevision := r.currentToolSchemaRevision()
	data := map[string]any{
		"capability_version": cleanCoreCapabilityVersion,
		"capability_groups":  capabilityGroups(),
		"limits":             publishedLimits(),
		"schema_source":      "tools/list",
		"agent_guidance":     guidance,
		"client_protocol":    clientProtocol,
		"workspace":          map[string]any{"name": ws.Name},
		"tools":              tools,
		"runtime": map[string]any{
			"version":                  r.build.Version,
			"build_commit":             r.build.Commit,
			"build_time":               r.build.Date,
			"tool_schema_revision":     toolSchemaRevision,
			"client_protocol_revision": clientProtocolRevision(),
			"capability_version":       cleanCoreCapabilityVersion,
			"capability_groups":        capabilityGroups(),
		},
		"instructions": map[string]any{
			"order": []string{"global", "project", "directory"}, "documents": r.agentInstructions(wsPath),
			"list_action": nextAction("runtime_read", map[string]any{"view": "instructions"}),
		},
		"extension_inventory": map[string]any{
			"skills":      compactSkillMaps(skills),
			"mcp_servers": compactMCPServerInventory(servers),
		},
		"resources": []map[string]any{
			{"kind": "task_logs", "uri_template": "mcpx://remote-sessions/{remote_session_id}/tasks/{execution_task_id}/logs", "mime_type": "text/plain"},
			{"kind": "artifact", "uri_template": "mcpx://remote-sessions/{remote_session_id}/artifacts/{artifact_id}"},
		},
		"recommended_workflows": map[string]any{
			"bootstrap":      []string{"workspace", "session"},
			"source_change":  []string{"read", "edit", "execute", "observe"},
			"plan_delivery":  []string{"plan", "edit", "execute", "artifact", "observe"},
			"extension_call": []string{"skill_tool", "mcp_tool"},
		},
	}
	instrDocs := r.agentInstructions(wsPath)
	data["revisions"] = map[string]any{
		"tool_schema_revision":         toolSchemaRevision,
		"capability_manifest_revision": capabilityManifestRevision(fullToolManifest, fullSkills, servers, instrDocs, guidance, clientProtocol),
		"guidance_revision":            agentGuidanceRevision(),
		"instruction_revision":         instructionRevision(instrDocs),
		"session_capability_revision":  sessionCapabilityRevision(session),
		"client_protocol_revision":     clientProtocolRevision(),
	}
	if session != nil {
		data["remote_session"] = map[string]any{"id": session.ID, "role": session.Role, "status": session.Status}
	}
	data["revision"] = capabilityRevision(data)
	r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remoteID, Workspace: ws.Name, Tool: "capability_list", Status: "ok"})
	return r.remoteResult(envReq, remoteID, ws.Name, data)
}

func (r *Runtime) toolWorkspaceList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return fail, nil
	}
	list := r.reg.List()
	items := make([]map[string]any, 0, len(list))
	for _, w := range list {
		items = append(items, map[string]any{
			"name":        w.Name,
			"path":        w.Path,
			"description": w.Description,
		})
	}
	r.logAudit(audit.Event{RequestID: envReq.RequestID, Tool: "workspace", Status: "ok"})
	return r.remoteResult(envReq, "", "", map[string]any{"workspaces": items})
}

func (r *Runtime) effectiveConfig(wsPath string) config.Config {
	if wsPath == "" {
		return r.cfg
	}
	path := config.ProjectConfigPath(wsPath)
	info, statErr := os.Stat(path)
	exists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return r.rejectProjectConfig(wsPath, statErr)
	}
	var modTime int64
	var size int64
	if exists {
		modTime = info.ModTime().UnixNano()
		size = info.Size()
	}

	r.projectConfigMu.RLock()
	cached, found := r.projectConfigs[path]
	r.projectConfigMu.RUnlock()
	if found && cached.exists == exists && cached.modTime == modTime && cached.size == size {
		if cached.err != nil {
			return r.failClosedProjectConfig()
		}
		return config.Merge(r.cfg, cached.config)
	}

	var proj config.Config
	var err error
	if exists {
		proj, err = config.LoadProject(wsPath)
	}
	entry := projectConfigCacheEntry{exists: exists, modTime: modTime, size: size, config: proj, err: err}
	r.projectConfigMu.Lock()
	if r.projectConfigs == nil {
		r.projectConfigs = make(map[string]projectConfigCacheEntry)
	}
	r.projectConfigs[path] = entry
	r.projectConfigMu.Unlock()
	if err != nil {
		return r.rejectProjectConfig(wsPath, err)
	}
	return config.Merge(r.cfg, proj)
}

func (r *Runtime) rejectProjectConfig(wsPath string, err error) config.Config {
	logging.With("component", "config").Error("project config rejected", "workspace", wsPath, "err", err)
	return r.failClosedProjectConfig()
}

func (r *Runtime) failClosedProjectConfig() config.Config {
	safe := r.cfg
	safe.Terminal.Enabled = false
	safe.FileWatch.Enabled = false
	safe.Discovery.MCP.Enabled = false
	safe.Discovery.Skills.Enabled = false
	safe.Security.Commands = config.CommandRules{Default: "deny"}
	safe.Security.Files = config.FileRules{Deny: []string{".*"}}
	return safe
}

func (r *Runtime) capExecResult(res terminal.Result) map[string]any {
	max := config.MaxResultBytes(r.cfg.Limits)
	stdout, trOut := TruncateUTF8(res.Stdout, max)
	stderr, trErr := TruncateUTF8(res.Stderr, max)
	out := map[string]any{
		"exit_code":   res.ExitCode,
		"stdout":      stdout,
		"stderr":      stderr,
		"duration_ms": res.DurationMs,
	}
	if trOut || trErr {
		out["truncated"] = true
	}
	return out
}
