package oauth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/google/uuid"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/LucianoR23/valuo_api/internal/audit"
	"github.com/LucianoR23/valuo_api/internal/auth"
	"github.com/LucianoR23/valuo_api/internal/domain"
	"github.com/LucianoR23/valuo_api/internal/httpx"
)

// PublicPrefixes are the paths assistants call from anywhere: their CORS is
// open and credential-free (they authenticate with bearer tokens or not at
// all). The app's own routes (/connections...) keep the app's CORS.
var PublicPrefixes = []string{"/.well-known/oauth-", "/oauth/", "/mcp"}

// Handler serves the authorization server and the app's routes.
type Handler struct {
	svc    *Service
	authMW *auth.Middleware
	audit  *audit.Recorder
	logger *slog.Logger
}

// NewHandler builds the handler. rec may be nil (no audit).
func NewHandler(svc *Service, authMW *auth.Middleware, rec *audit.Recorder, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, authMW: authMW, audit: rec, logger: logger}
}

// Mount adds the routes.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/.well-known/oauth-authorization-server", h.serverMetadata)
	prm := mcpauth.ProtectedResourceMetadataHandler(h.resourceMetadata())
	r.Method(http.MethodGet, "/.well-known/oauth-protected-resource", prm)
	r.Method(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", prm)

	r.With(httprate.LimitBy(20, time.Hour, httpx.ClientIPKey)).Post("/oauth/register", h.register)
	r.With(httprate.LimitBy(30, time.Minute, httpx.ClientIPKey)).Get("/oauth/authorize", h.authorize)
	r.With(httprate.LimitBy(60, time.Minute, httpx.ClientIPKey)).Post("/oauth/token", h.token)
	r.With(httprate.LimitBy(60, time.Minute, httpx.ClientIPKey)).Post("/oauth/revoke", h.revoke)

	r.Group(func(r chi.Router) {
		r.Use(h.authMW.RequireAuth)
		r.Use(h.authMW.RequireVerifiedEmail)
		r.Get("/connection-requests/{id}", h.getRequest)
		r.Post("/connection-requests/{id}", h.decide)
		r.Get("/connections", h.connections)
		r.Delete("/connections/{id}", h.disconnect)
	})
}

// ResourceMetadataURL is where a 401 from /mcp points clients (RFC 9728).
func (h *Handler) ResourceMetadataURL() string {
	return h.svc.Issuer() + "/.well-known/oauth-protected-resource/mcp"
}

func (h *Handler) resourceMetadata() *oauthex.ProtectedResourceMetadata {
	return &oauthex.ProtectedResourceMetadata{
		Resource:               h.svc.Resource(),
		AuthorizationServers:   []string{h.svc.Issuer()},
		ScopesSupported:        SupportedScopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Valuo",
	}
}

func (h *Handler) serverMetadata(w http.ResponseWriter, _ *http.Request) {
	iss := h.svc.Issuer()
	methods := []string{authNone, authSecretPost, authSecretBase}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         iss,
		"authorization_endpoint":                         iss + "/oauth/authorize",
		"token_endpoint":                                 iss + "/oauth/token",
		"registration_endpoint":                          iss + "/oauth/register",
		"revocation_endpoint":                            iss + "/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          methods,
		"revocation_endpoint_auth_methods_supported":     methods,
		"scopes_supported":                               SupportedScopes,
		"authorization_response_iss_parameter_supported": true,
	})
}

func writeOAuthError(w http.ResponseWriter, err *Error) {
	status := err.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="oauth"`)
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, status, map[string]string{"error": err.Code, "error_description": err.Description})
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var oe *Error
	if errors.As(err, &oe) {
		writeOAuthError(w, oe)
		return
	}
	h.logger.ErrorContext(r.Context(), "oauth", "error", err)
	writeOAuthError(w, &Error{Code: "server_error", Description: "try again later", Status: http.StatusInternalServerError})
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var in RegisterInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeOAuthError(w, oauthErr("invalid_client_metadata", "the body must be JSON client metadata"))
		return
	}
	c, err := h.svc.Register(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, c)
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) {
	to, err := h.svc.Authorize(r.Context(), r.URL.Query())
	var re *RedirectError
	if errors.As(err, &re) {
		// Nowhere safe to send the browser back: show it (RFC 6749 §4.1.2.1).
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("No pudimos conectar tu asistente: el pedido no es válido. " + re.Description))
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "oauth authorize", "error", err)
		http.Error(w, "No pudimos conectar tu asistente. Probá de nuevo en un rato.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, to, http.StatusFound) //nolint:gosec // G710: "to" is APP_BASE_URL/conectar or a redirect_uri registered by the client and matched exactly
}

// credentials reads the client's authentication (RFC 6749 §2.3.1).
func credentials(r *http.Request) (ClientCredentials, bool) {
	if id, secret, ok := r.BasicAuth(); ok {
		uid, err1 := url.QueryUnescape(id)
		usecret, err2 := url.QueryUnescape(secret)
		if err1 != nil || err2 != nil || (r.PostForm.Get("client_id") != "" && r.PostForm.Get("client_id") != uid) {
			return ClientCredentials{}, false
		}
		return ClientCredentials{ID: uid, Secret: usecret, Basic: true}, true
	}
	return ClientCredentials{ID: r.PostForm.Get("client_id"), Secret: r.PostForm.Get("client_secret")}, true
}

func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, oauthErr("invalid_request", "the body must be form-encoded"))
		return
	}
	cred, ok := credentials(r)
	if !ok {
		writeOAuthError(w, &Error{Code: "invalid_client", Description: "client authentication failed", Status: 401})
		return
	}
	var (
		tok   TokenResponse
		reuse *Reuse
		err   error
	)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		tok, reuse, err = h.svc.ExchangeCode(r.Context(), cred, r.PostForm.Get("code"), r.PostForm.Get("redirect_uri"), r.PostForm.Get("code_verifier"))
	case "refresh_token":
		tok, reuse, err = h.svc.Refresh(r.Context(), cred, r.PostForm.Get("refresh_token"), r.PostForm.Get("scope"))
	default:
		err = oauthErr("unsupported_grant_type", "use authorization_code or refresh_token")
	}
	if reuse != nil {
		h.record(r, reuse.UserID, audit.OAuthReuse, reuse.ClientID, map[string]any{"connection": reuse.GrantID.String()})
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	httpx.WriteJSON(w, http.StatusOK, tok)
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, oauthErr("invalid_request", "the body must be form-encoded"))
		return
	}
	cred, ok := credentials(r)
	if !ok {
		writeOAuthError(w, &Error{Code: "invalid_client", Description: "client authentication failed", Status: 401})
		return
	}
	if _, err := h.svc.Revoke(r.Context(), cred, r.PostForm.Get("token")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) record(r *http.Request, userID uuid.UUID, action, target string, meta map[string]any) {
	if h.audit == nil || userID == uuid.Nil {
		return
	}
	h.audit.Record(r.Context(), r, audit.Event{UserID: &userID, ActorID: &userID, Action: action, Target: target, Metadata: meta})
}

func idParam(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, domain.ErrNotFound
	}
	return id, nil
}

func (h *Handler) getRequest(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err == nil {
		var req Request
		if req, err = h.svc.GetRequest(r.Context(), id); err == nil {
			httpx.WriteJSON(w, http.StatusOK, req)
			return
		}
	}
	httpx.WriteError(w, r, h.logger, err)
}

func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, h.logger, domain.ErrUnauthorized)
		return
	}
	id, err := idParam(r)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	var body struct {
		Approve *bool `json:"approve"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Approve == nil {
		httpx.WriteError(w, r, h.logger, domain.NewValidationError("approve", "es obligatorio"))
		return
	}
	d, err := h.svc.Decide(r.Context(), userID, id, *body.Approve)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	if *body.Approve {
		h.record(r, userID, audit.OAuthConnected, d.ClientID, map[string]any{"connection": d.GrantID.String()})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"redirectTo": d.RedirectTo})
}

func (h *Handler) connections(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, h.logger, domain.ErrUnauthorized)
		return
	}
	list, err := h.svc.Connections(r.Context(), userID)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (h *Handler) disconnect(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, h.logger, domain.ErrUnauthorized)
		return
	}
	id, err := idParam(r)
	if err == nil {
		err = h.svc.Disconnect(r.Context(), userID, id)
	}
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	h.record(r, userID, audit.OAuthDisconnected, "", map[string]any{"connection": id.String()})
	w.WriteHeader(http.StatusNoContent)
}

// IsPublic tells whether a path is one of PublicPrefixes.
func IsPublic(path string) bool {
	for _, p := range PublicPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
