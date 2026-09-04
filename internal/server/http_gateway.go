package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"mcpx/internal/auth"
	"mcpx/internal/config"
	"mcpx/internal/logging"
	"mcpx/internal/oauth"
)

// Gateway fronts Streamable MCP with OAuth discovery and auth middleware.
type Gateway struct {
	cfg        config.Config
	oauth      *oauth.Server
	oauthHTTP  *oauth.Handler
	mcp        http.Handler
	trustProxy bool
}

// NewGateway builds the HTTP front door.
func NewGateway(cfg config.Config, oauthSrv *oauth.Server, mcp http.Handler) *Gateway {
	g := &Gateway{
		cfg:        cfg,
		oauth:      oauthSrv,
		mcp:        mcp,
		trustProxy: cfg.Server.TrustProxyHeaders,
	}
	if oauthSrv != nil {
		g.oauthHTTP = &oauth.Handler{S: oauthSrv}
	}
	return g
}

// Handler returns the root mux.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	if g.oauthHTTP != nil {
		h := g.oauthHTTP
		// RFC9728 PRM — root and path-qualified (resource ends with /mcp)
		prm := g.cors(h.HandleProtectedResourceMetadata)
		mux.HandleFunc("/.well-known/oauth-protected-resource", prm)
		mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", prm)

		// RFC8414 AS metadata + OpenID discovery alias (clients try both)
		asMeta := g.cors(h.HandleAuthorizationServerMetadata)
		mux.HandleFunc("/.well-known/oauth-authorization-server", asMeta)
		mux.HandleFunc("/.well-known/openid-configuration", asMeta)
		// Path-insert form when some clients treat resource URL as issuer base
		mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", asMeta)
		mux.HandleFunc("/.well-known/openid-configuration/mcp", asMeta)
		mux.HandleFunc("/mcp/.well-known/oauth-authorization-server", asMeta)
		mux.HandleFunc("/mcp/.well-known/openid-configuration", asMeta)

		// OAuth endpoints share the MCP path prefix for reverse-proxy-friendly routing.
		mux.HandleFunc(oauth.MCPOAuthPrefix+"/register", g.cors(h.HandleRegister))
		mux.HandleFunc(oauth.MCPOAuthPrefix+"/authorize", g.cors(h.HandleAuthorize))
		mux.HandleFunc(oauth.MCPOAuthPrefix+"/token", g.cors(h.HandleToken))
	}
	// Exact /mcp only for Streamable MCP. /mcp/oauth/* registered above wins (longer path).
	mux.Handle("/mcp", g.corsHandler(g.accessLog(g.wrapMCP(g.mcp))))
	return mux
}

func (g *Gateway) wrapMCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := config.EffectiveAuthMode(g.cfg.Auth)
		origin := g.origin(r)
		issuer := origin
		resource := origin + "/mcp"
		if g.oauth != nil {
			issuer = g.oauth.EffectiveIssuer(origin)
			resource = g.oauth.ResourceURL(issuer)
		} else if g.cfg.Auth.OAuth.ServerURL != "" {
			issuer = strings.TrimRight(g.cfg.Auth.OAuth.ServerURL, "/")
			resource = issuer + "/mcp"
		}

		cred := auth.ValidateHTTP(
			r.Header.Get("Authorization"),
			mode,
			strings.TrimSpace(g.cfg.Auth.Token),
			g.oauth,
			issuer,
			resource,
		)
		if !cred.OK {
			authHeader := r.Header.Get("Authorization")
			logging.L().Info("mcp auth denied",
				"component", "mcp_http", "method", r.Method, "path", r.URL.Path,
				"mode", mode, "has_auth", authHeader != "",
				"auth_len", len(authHeader),
				"issuer", issuer, "resource", resource,
				"mcp_transport_session_hash", privacySafeHash(r.Header.Get("Mcp-Session-Id")))
			// Prefer path-qualified metadata URL (MCP resource is …/mcp)
			metaURL := issuer + "/.well-known/oauth-protected-resource/mcp"
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="mcpx", resource_metadata=%q, scope=%q`, metaURL, oauth.DefaultScope))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Inject auth + runtime context for tool handlers (official SDK has no
		// WithHTTPContextFunc equivalent on StreamableHTTPHandler).
		ctx := auth.ContextWithAuthorization(r.Context(), r.Header.Get("Authorization"))
		ctx, _ = ensureRuntimeContext(ctx, r.Header, time.Now())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (g *Gateway) origin(r *http.Request) string {
	return oauth.OriginFromRequest(r, g.trustProxy)
}

func (g *Gateway) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.applyCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func (g *Gateway) corsHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.applyCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	allowed := g.cfg.Server.AllowedOrigins
	ok := false
	if len(allowed) == 0 {
		ok = true // reflect for web dogfood; document risk
	} else {
		for _, a := range allowed {
			if a == "*" || a == origin {
				ok = true
				break
			}
		}
	}
	if !ok {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Session-Id, MCP-Protocol-Version, Accept, X-Request-ID, X-MCPX-Request-ID, Traceparent, X-MCPX-Trace-ID, X-MCPX-Span-ID, X-MCPX-Started-At-Ms")
	w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, X-Request-ID, X-MCPX-Trace-ID, X-MCPX-Span-ID")
}
