// Package oauth is the API's own OAuth 2.1 authorization server (plan 8.6),
// so a person can connect their own AI assistant (Claude, ChatGPT) to the
// MCP server at /mcp.
//
// It covers only what those assistants use, decided with the user on
// 2026-11-16 instead of a library: authorization code with PKCE S256
// (required), clients that register themselves (RFC 7591), refresh tokens
// that rotate, revocation (RFC 7009) and the metadata documents (RFC 8414,
// RFC 9728). Codes and tokens are 32 random bytes; only their SHA-256 is
// stored. A code used twice or a rotated refresh token used again revokes
// the whole connection (RFC 9700).
//
// The person approves in the app: /oauth/authorize keeps the request and
// sends the browser to <app>/conectar, which shows it and answers through
// the API with the person's normal session.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	sqlcgen "github.com/LucianoR23/valuo_api/internal/db/sqlc"
	"github.com/LucianoR23/valuo_api/internal/domain"
)

// Scopes. finance:write_drafts arrives with 8.6b.
const (
	ScopeRead = "finance:read"
)

// SupportedScopes are the scopes a client may ask for.
var SupportedScopes = []string{ScopeRead}

// Lifetimes.
const (
	requestTTL = 10 * time.Minute
	codeTTL    = 2 * time.Minute
	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
	// clientTTL: a client nobody connected within this time is deleted.
	clientTTL = 24 * time.Hour
)

// Token endpoint authentication methods.
const (
	authNone       = "none"
	authSecretPost = "client_secret_post"
	authSecretBase = "client_secret_basic"
)

// Error is an OAuth error response (RFC 6749 §5.2).
type Error struct {
	Code        string
	Description string
	// Status: 400 unless set (401 for invalid_client).
	Status int
}

func (e *Error) Error() string { return e.Code + ": " + e.Description }

func oauthErr(code, desc string) *Error { return &Error{Code: code, Description: desc} }

// UserLookup tells whether an account still exists and is verified.
type UserLookup interface {
	GetByID(ctx context.Context, id uuid.UUID) (domain.User, error)
}

// Config of the server.
type Config struct {
	// Issuer is the API's public origin (API_PUBLIC_URL).
	Issuer string
	// AppBaseURL is the app, where the person approves (/conectar).
	AppBaseURL string
	Now        func() time.Time
	Logger     *slog.Logger
}

// Service runs the authorization server.
type Service struct {
	pool  *pgxpool.Pool
	q     *sqlcgen.Queries
	users UserLookup
	cfg   Config
}

// NewService builds the server.
func NewService(pool *pgxpool.Pool, users UserLookup, cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	cfg.AppBaseURL = strings.TrimRight(cfg.AppBaseURL, "/")
	return &Service{pool: pool, q: sqlcgen.New(pool), users: users, cfg: cfg}
}

// Resource is the MCP server's URL, the audience of every token.
func (s *Service) Resource() string { return s.cfg.Issuer + "/mcp" }

// Issuer is the authorization server's identifier.
func (s *Service) Issuer() string { return s.cfg.Issuer }

func (s *Service) now() time.Time { return s.cfg.Now() }

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// newSecret returns 32 random bytes as base64url and their SHA-256.
func newSecret() (string, []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand never fails on supported platforms
	tok := base64.RawURLEncoding.EncodeToString(b)
	return tok, hash(tok)
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// ---------- registration (RFC 7591) ----------

// RegisterInput is the client metadata a client sends.
type RegisterInput struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

// Client is the registration response.
type Client struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientSecretExpiresAt   *int64   `json:"client_secret_expires_at,omitempty"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

// Register creates a client.
func (s *Service) Register(ctx context.Context, in RegisterInput) (Client, error) {
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 5 {
		return Client{}, oauthErr("invalid_redirect_uri", "between 1 and 5 redirect_uris are required")
	}
	for _, u := range in.RedirectURIs {
		if err := checkRedirectURI(u); err != nil {
			return Client{}, err
		}
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return Client{}, oauthErr("invalid_client_metadata", "unsupported grant_type "+g)
		}
	}
	for _, r := range in.ResponseTypes {
		if r != "code" {
			return Client{}, oauthErr("invalid_client_metadata", "unsupported response_type "+r)
		}
	}
	method := in.TokenEndpointAuthMethod
	switch method {
	case "":
		method = authNone
	case authNone, authSecretPost, authSecretBase:
	default:
		return Client{}, oauthErr("invalid_client_metadata", "unsupported token_endpoint_auth_method")
	}
	name := strings.TrimSpace(in.ClientName)
	if name == "" {
		name = "Asistente"
	}
	if r := []rune(name); len(r) > 100 {
		name = string(r[:100])
	}

	id, _ := newSecret()
	id = "vc_" + id[:24]
	out := Client{
		ClientID:                id,
		ClientName:              name,
		RedirectURIs:            in.RedirectURIs,
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: method,
		Scope:                   strings.Join(SupportedScopes, " "),
	}
	var secretHash []byte
	if method != authNone {
		out.ClientSecret, secretHash = newSecret()
		never := int64(0)
		out.ClientSecretExpiresAt = &never
	}
	c, err := s.q.CreateOAuthClient(ctx, sqlcgen.CreateOAuthClientParams{
		ID: id, ClientName: name, RedirectUris: in.RedirectURIs, AuthMethod: method, SecretHash: secretHash,
	})
	if err != nil {
		return Client{}, fmt.Errorf("oauth: register: %w", err)
	}
	out.ClientIDIssuedAt = c.CreatedAt.Time.Unix()
	return out, nil
}

// checkRedirectURI accepts https URLs and http on loopback (desktop apps and
// MCP Inspector, RFC 8252), without fragments or user info.
func checkRedirectURI(raw string) error {
	bad := oauthErr("invalid_redirect_uri", "redirect_uri must be https (or http on localhost) without a fragment")
	if len(raw) > 2000 {
		return bad
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil {
		return bad
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	return bad
}

// ---------- authorization ----------

// RedirectError is an authorization error that cannot go back to the client
// (unknown client or redirect_uri): the browser shows it instead.
type RedirectError struct{ Description string }

func (e *RedirectError) Error() string { return e.Description }

// Authorize checks an authorization request and keeps it. It returns where
// to send the browser: the app's consent screen, or the client's
// redirect_uri with an error.
func (s *Service) Authorize(ctx context.Context, q url.Values) (string, error) {
	clientID := q.Get("client_id")
	client, err := s.q.GetOAuthClient(ctx, clientID)
	if errors.Is(err, pgx.ErrNoRows) || clientID == "" {
		return "", &RedirectError{"Unknown client_id."}
	}
	if err != nil {
		return "", fmt.Errorf("oauth: client: %w", err)
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" && len(client.RedirectUris) == 1 {
		redirectURI = client.RedirectUris[0]
	}
	if !slices.Contains(client.RedirectUris, redirectURI) {
		return "", &RedirectError{"redirect_uri is not registered for this client."}
	}
	state := q.Get("state")
	fail := func(code, desc string) (string, error) {
		return s.errorRedirect(redirectURI, state, code, desc), nil
	}
	if q.Get("response_type") != "code" {
		return fail("unsupported_response_type", "only response_type=code")
	}
	challenge := q.Get("code_challenge")
	if q.Get("code_challenge_method") != "S256" || !validChallenge(challenge) {
		return fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
	}
	if res := q.Get("resource"); res != "" && strings.TrimRight(res, "/") != s.Resource() {
		return fail("invalid_target", "unknown resource")
	}
	scopes, ok := parseScopes(q.Get("scope"))
	if !ok {
		return fail("invalid_scope", "supported scopes: "+strings.Join(SupportedScopes, " "))
	}
	if len(state) > 500 {
		return fail("invalid_request", "state is too long")
	}
	id, err := s.q.CreateOAuthRequest(ctx, sqlcgen.CreateOAuthRequestParams{
		ClientID: client.ID, RedirectUri: redirectURI, Scopes: scopes, State: state,
		CodeChallenge: challenge, ExpiresAt: ts(s.now().Add(requestTTL)),
	})
	if err != nil {
		return "", fmt.Errorf("oauth: keep request: %w", err)
	}
	return s.cfg.AppBaseURL + "/conectar?pedido=" + id.String(), nil
}

// validChallenge: a base64url SHA-256 is 43 characters (RFC 7636 allows up
// to 128 for other methods).
func validChallenge(c string) bool {
	if len(c) < 43 || len(c) > 128 {
		return false
	}
	for _, r := range c {
		if !unreserved(r) {
			return false
		}
	}
	return true
}

// unreserved: the characters RFC 7636 allows in a verifier or challenge.
func unreserved(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return r == '-' || r == '_' || r == '.' || r == '~'
}

// parseScopes: empty means finance:read; unknown scopes fail.
func parseScopes(raw string) ([]string, bool) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return []string{ScopeRead}, true
	}
	var out []string
	for _, f := range fields {
		if !slices.Contains(SupportedScopes, f) {
			return nil, false
		}
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out, true
}

func (s *Service) errorRedirect(redirectURI, state, code, desc string) string {
	v := url.Values{"error": {code}, "error_description": {desc}, "iss": {s.cfg.Issuer}}
	if state != "" {
		v.Set("state", state)
	}
	return withQuery(redirectURI, v)
}

func withQuery(raw string, v url.Values) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, vs := range v {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Request is what the consent screen shows.
type Request struct {
	ID           uuid.UUID `json:"id"`
	ClientName   string    `json:"clientName"`
	RedirectHost string    `json:"redirectHost"`
	Scopes       []string  `json:"scopes"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// GetRequest returns a pending request (any signed-in person may answer it;
// whoever approves is the account connected).
func (s *Service) GetRequest(ctx context.Context, id uuid.UUID) (Request, error) {
	r, err := s.q.GetOAuthRequest(ctx, sqlcgen.GetOAuthRequestParams{ID: id, ExpiresAt: ts(s.now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, domain.ErrNotFound
	}
	if err != nil {
		return Request{}, fmt.Errorf("oauth: request: %w", err)
	}
	host := ""
	if u, err := url.Parse(r.RedirectUri); err == nil {
		host = u.Host
	}
	return Request{ID: r.ID, ClientName: r.ClientName, RedirectHost: host, Scopes: r.Scopes, ExpiresAt: r.ExpiresAt.Time}, nil
}

// Decision is the result of answering a request.
type Decision struct {
	RedirectTo string
	// GrantID and ClientID are set when the person approved.
	GrantID  uuid.UUID
	ClientID string
}

// Decide answers a pending request for userID. Approving creates (or
// renews) the connection and a single-use code; either way the request is
// gone and the browser goes back to the client.
func (s *Service) Decide(ctx context.Context, userID, id uuid.UUID, approve bool) (Decision, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Decision{}, fmt.Errorf("oauth: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	r, err := q.DeleteOAuthRequest(ctx, sqlcgen.DeleteOAuthRequestParams{ID: id, ExpiresAt: ts(s.now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, domain.ErrNotFound
	}
	if err != nil {
		return Decision{}, fmt.Errorf("oauth: take request: %w", err)
	}
	if !approve {
		if err := tx.Commit(ctx); err != nil {
			return Decision{}, fmt.Errorf("oauth: commit: %w", err)
		}
		return Decision{RedirectTo: s.errorRedirect(r.RedirectUri, r.State, "access_denied", "the person did not allow the connection")}, nil
	}
	grantID, err := q.UpsertOAuthGrant(ctx, sqlcgen.UpsertOAuthGrantParams{UserID: userID, ClientID: r.ClientID, Scopes: r.Scopes})
	if err != nil {
		return Decision{}, fmt.Errorf("oauth: grant: %w", err)
	}
	code, codeHash := newSecret()
	if err := q.CreateOAuthCode(ctx, sqlcgen.CreateOAuthCodeParams{
		CodeHash: codeHash, GrantID: grantID, RedirectUri: r.RedirectUri,
		CodeChallenge: r.CodeChallenge, Scopes: r.Scopes, ExpiresAt: ts(s.now().Add(codeTTL)),
	}); err != nil {
		return Decision{}, fmt.Errorf("oauth: code: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, fmt.Errorf("oauth: commit: %w", err)
	}
	v := url.Values{"code": {code}, "iss": {s.cfg.Issuer}}
	if r.State != "" {
		v.Set("state", r.State)
	}
	return Decision{RedirectTo: withQuery(r.RedirectUri, v), GrantID: grantID, ClientID: r.ClientID}, nil
}

// ---------- token endpoint ----------

// TokenResponse is the token endpoint's success body.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// ClientCredentials is how the client authenticated at the token endpoint.
type ClientCredentials struct {
	ID     string
	Secret string
	// Basic: the credentials came in the Authorization header.
	Basic bool
}

// Reuse reports a revoked connection (code or refresh token used twice).
type Reuse struct {
	UserID   uuid.UUID
	GrantID  uuid.UUID
	ClientID string
}

// authClient checks the client's credentials against its registration.
func (s *Service) authClient(ctx context.Context, cred ClientCredentials) (sqlcgen.OauthClient, error) {
	invalid := &Error{Code: "invalid_client", Description: "client authentication failed", Status: 401}
	if cred.ID == "" {
		return sqlcgen.OauthClient{}, invalid
	}
	c, err := s.q.GetOAuthClient(ctx, cred.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, invalid
	}
	if err != nil {
		return c, fmt.Errorf("oauth: client: %w", err)
	}
	switch c.AuthMethod {
	case authNone:
		if cred.Secret != "" {
			return c, invalid
		}
	case authSecretPost, authSecretBase:
		if (c.AuthMethod == authSecretBase) != cred.Basic || cred.Secret == "" ||
			subtle.ConstantTimeCompare(hash(cred.Secret), c.SecretHash) != 1 {
			return c, invalid
		}
	}
	return c, nil
}

// ExchangeCode trades an authorization code for tokens. A code presented a
// second time revokes the connection and returns its Reuse.
func (s *Service) ExchangeCode(ctx context.Context, cred ClientCredentials, code, redirectURI, verifier string) (TokenResponse, *Reuse, error) {
	client, err := s.authClient(ctx, cred)
	if err != nil {
		return TokenResponse{}, nil, err
	}
	invalid := oauthErr("invalid_grant", "the code is invalid, expired or already used")
	if code == "" {
		return TokenResponse{}, nil, invalid
	}
	now := s.now()
	row, err := s.q.UseOAuthCode(ctx, sqlcgen.UseOAuthCodeParams{CodeHash: hash(code), UsedAt: ts(now)})
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenResponse{}, nil, invalid
	}
	if err != nil {
		return TokenResponse{}, nil, fmt.Errorf("oauth: use code: %w", err)
	}
	if row.ClientID != client.ID {
		return TokenResponse{}, nil, invalid
	}
	if row.PrevUsedAt.Valid {
		if err := s.q.DeleteOAuthGrant(ctx, row.GrantID); err != nil {
			return TokenResponse{}, nil, fmt.Errorf("oauth: revoke on reuse: %w", err)
		}
		return TokenResponse{}, &Reuse{UserID: row.UserID, GrantID: row.GrantID, ClientID: row.ClientID}, invalid
	}
	if !row.ExpiresAt.Time.After(now) || row.RedirectUri != redirectURI || !pkceMatches(verifier, row.CodeChallenge) {
		return TokenResponse{}, nil, invalid
	}
	if err := s.activeUser(ctx, row.UserID); err != nil {
		return TokenResponse{}, nil, err
	}
	tok, err := s.issue(ctx, s.q, row.GrantID, row.Scopes, now)
	return tok, nil, err
}

// Refresh rotates a refresh token. A rotated token presented again revokes
// the connection and returns its Reuse.
func (s *Service) Refresh(ctx context.Context, cred ClientCredentials, refreshToken, scope string) (TokenResponse, *Reuse, error) {
	client, err := s.authClient(ctx, cred)
	if err != nil {
		return TokenResponse{}, nil, err
	}
	invalid := oauthErr("invalid_grant", "the refresh token is invalid or expired")
	if refreshToken == "" {
		return TokenResponse{}, nil, invalid
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenResponse{}, nil, fmt.Errorf("oauth: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	row, err := q.RotateOAuthRefreshToken(ctx, sqlcgen.RotateOAuthRefreshTokenParams{TokenHash: hash(refreshToken), RotatedAt: ts(now)})
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenResponse{}, nil, invalid
	}
	if err != nil {
		return TokenResponse{}, nil, fmt.Errorf("oauth: rotate: %w", err)
	}
	if row.ClientID != client.ID {
		return TokenResponse{}, nil, invalid
	}
	if row.PrevRotatedAt.Valid {
		if err := q.DeleteOAuthGrant(ctx, row.GrantID); err != nil {
			return TokenResponse{}, nil, fmt.Errorf("oauth: revoke on reuse: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenResponse{}, nil, fmt.Errorf("oauth: commit: %w", err)
		}
		return TokenResponse{}, &Reuse{UserID: row.UserID, GrantID: row.GrantID, ClientID: row.ClientID}, invalid
	}
	if !row.ExpiresAt.Time.After(now) {
		return TokenResponse{}, nil, invalid
	}
	scopes := row.Scopes
	if scope != "" {
		asked, ok := parseScopes(scope)
		if !ok {
			return TokenResponse{}, nil, oauthErr("invalid_scope", "unknown scope")
		}
		for _, a := range asked {
			if !slices.Contains(row.Scopes, a) {
				return TokenResponse{}, nil, oauthErr("invalid_scope", "a refresh cannot widen the scopes")
			}
		}
		scopes = asked
	}
	if err := s.activeUser(ctx, row.UserID); err != nil {
		return TokenResponse{}, nil, err
	}
	tok, err := s.issue(ctx, q, row.GrantID, scopes, now)
	if err != nil {
		return TokenResponse{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenResponse{}, nil, fmt.Errorf("oauth: commit: %w", err)
	}
	return tok, nil, nil
}

// activeUser: a deleted or unverified account gets no tokens.
func (s *Service) activeUser(ctx context.Context, userID uuid.UUID) error {
	u, err := s.users.GetByID(ctx, userID)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && u.EmailVerifiedAt == nil) {
		return oauthErr("invalid_grant", "the account is not available")
	}
	if err != nil {
		return fmt.Errorf("oauth: user: %w", err)
	}
	return nil
}

func (s *Service) issue(ctx context.Context, q *sqlcgen.Queries, grantID uuid.UUID, scopes []string, now time.Time) (TokenResponse, error) {
	access, accessHash := newSecret()
	refresh, refreshHash := newSecret()
	if err := q.CreateOAuthToken(ctx, sqlcgen.CreateOAuthTokenParams{
		TokenHash: accessHash, GrantID: grantID, Kind: "access", Scopes: scopes, ExpiresAt: ts(now.Add(accessTTL)),
	}); err != nil {
		return TokenResponse{}, fmt.Errorf("oauth: access token: %w", err)
	}
	if err := q.CreateOAuthToken(ctx, sqlcgen.CreateOAuthTokenParams{
		TokenHash: refreshHash, GrantID: grantID, Kind: "refresh", Scopes: scopes, ExpiresAt: ts(now.Add(refreshTTL)),
	}); err != nil {
		return TokenResponse{}, fmt.Errorf("oauth: refresh token: %w", err)
	}
	return TokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int(accessTTL.Seconds()),
		RefreshToken: refresh, Scope: strings.Join(scopes, " "),
	}, nil
}

func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// Revoke ends the connection a token belongs to (RFC 7009). Unknown tokens
// are not an error.
func (s *Service) Revoke(ctx context.Context, cred ClientCredentials, token string) (*Reuse, error) {
	client, err := s.authClient(ctx, cred)
	if err != nil {
		return nil, err
	}
	g, err := s.q.GetOAuthTokenGrant(ctx, hash(token))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && g.ClientID != client.ID) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: revoke lookup: %w", err)
	}
	if err := s.q.DeleteOAuthGrant(ctx, g.ID); err != nil {
		return nil, fmt.Errorf("oauth: revoke: %w", err)
	}
	return &Reuse{GrantID: g.ID, ClientID: g.ClientID}, nil
}

// ---------- resource side ----------

// Access is a verified access token.
type Access struct {
	UserID    uuid.UUID
	GrantID   uuid.UUID
	ClientID  string
	Scopes    []string
	ExpiresAt time.Time
}

// ErrInvalidToken: unknown, expired or revoked token, or an account that is
// no longer available.
var ErrInvalidToken = errors.New("oauth: invalid token")

// Verify checks an access token on every MCP call, and that its account
// still exists and is verified.
func (s *Service) Verify(ctx context.Context, token string) (Access, error) {
	row, err := s.q.GetOAuthAccessToken(ctx, sqlcgen.GetOAuthAccessTokenParams{TokenHash: hash(token), ExpiresAt: ts(s.now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return Access{}, ErrInvalidToken
	}
	if err != nil {
		return Access{}, fmt.Errorf("oauth: verify: %w", err)
	}
	if err := s.activeUser(ctx, row.UserID); err != nil {
		var oe *Error
		if errors.As(err, &oe) {
			return Access{}, ErrInvalidToken
		}
		return Access{}, err
	}
	return Access{UserID: row.UserID, GrantID: row.GrantID, ClientID: row.ClientID, Scopes: row.Scopes, ExpiresAt: row.ExpiresAt.Time}, nil
}

// ---------- connections (/ajustes/conexiones) ----------

// Connection is one connected assistant.
type Connection struct {
	ID         uuid.UUID  `json:"id"`
	ClientName string     `json:"clientName"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

// Connections lists a person's connections, newest first.
func (s *Service) Connections(ctx context.Context, userID uuid.UUID) ([]Connection, error) {
	rows, err := s.q.ListOAuthGrantsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("oauth: connections: %w", err)
	}
	out := make([]Connection, len(rows))
	for i, r := range rows {
		out[i] = Connection{ID: r.ID, ClientName: r.ClientName, Scopes: r.Scopes, CreatedAt: r.CreatedAt.Time}
		if r.LastUsedAt.Valid {
			t := r.LastUsedAt.Time
			out[i].LastUsedAt = &t
		}
	}
	return out, nil
}

// Disconnect revokes one of the person's connections and its tokens.
func (s *Service) Disconnect(ctx context.Context, userID, id uuid.UUID) error {
	n, err := s.q.DeleteOAuthGrantForUser(ctx, sqlcgen.DeleteOAuthGrantForUserParams{ID: id, UserID: userID})
	if err != nil {
		return fmt.Errorf("oauth: disconnect: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Cleanup deletes expired requests, codes, tokens and call counters, and
// clients nobody connected within a day (hourly job).
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	now := s.now()
	var total int64
	steps := []func() (int64, error){
		func() (int64, error) { return s.q.CleanupOAuthRequests(ctx, ts(now)) },
		func() (int64, error) { return s.q.CleanupOAuthCodes(ctx, ts(now)) },
		func() (int64, error) { return s.q.CleanupOAuthTokens(ctx, ts(now)) },
		func() (int64, error) { return s.q.CleanupOAuthClients(ctx, ts(now.Add(-clientTTL))) },
		func() (int64, error) { return s.q.CleanupMCPCalls(ctx, ts(now.Add(-48*time.Hour))) },
	}
	for _, step := range steps {
		n, err := step()
		if err != nil {
			return total, fmt.Errorf("oauth: cleanup: %w", err)
		}
		total += n
	}
	return total, nil
}

// TouchGrant records the connection's last use.
func (s *Service) TouchGrant(ctx context.Context, grantID uuid.UUID) error {
	return s.q.TouchOAuthGrant(ctx, sqlcgen.TouchOAuthGrantParams{ID: grantID, LastUsedAt: ts(s.now())})
}

// CountCall adds one MCP call to the connection and returns the calls of the
// current hour and of the last 24 hours.
func (s *Service) CountCall(ctx context.Context, grantID uuid.UUID) (hour, day int, err error) {
	row, err := s.q.CountMCPCall(ctx, sqlcgen.CountMCPCallParams{GrantID: grantID, Column2: ts(s.now().Truncate(time.Hour))})
	if err != nil {
		return 0, 0, fmt.Errorf("oauth: count call: %w", err)
	}
	return int(row.HourCount), int(row.DayCount), nil
}
