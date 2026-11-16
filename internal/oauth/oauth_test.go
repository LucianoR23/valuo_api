package oauth_test

// The OAuth 2.1 server (plan 8.6) against the real API and Postgres.

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LucianoR23/valuo_api/internal/testutil/apitest"
	"github.com/LucianoR23/valuo_api/internal/testutil/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func TestMetadata(t *testing.T) {
	s := apitest.New(t)

	var as map[string]any
	s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/.well-known/oauth-authorization-server"}).Status(t, http.StatusOK).JSON(t, &as)
	for k, want := range map[string]any{
		"issuer":                 "https://api.test",
		"authorization_endpoint": "https://api.test/oauth/authorize",
		"token_endpoint":         "https://api.test/oauth/token",
		"registration_endpoint":  "https://api.test/oauth/register",
	} {
		if as[k] != want {
			t.Errorf("%s = %v, want %v", k, as[k], want)
		}
	}
	if m := as["code_challenge_methods_supported"].([]any); len(m) != 1 || m[0] != "S256" {
		t.Errorf("PKCE methods = %v, want only S256", m)
	}
	if _, cimd := as["client_id_metadata_document_supported"]; cimd {
		t.Error("CIMD is not supported and must not be advertised")
	}

	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		var prm map[string]any
		res := s.Do(t, apitest.Req{Method: http.MethodGet, Path: path, Header: http.Header{"Origin": {"https://claude.ai"}}})
		res.Status(t, http.StatusOK).JSON(t, &prm)
		if prm["resource"] != "https://api.test/mcp" {
			t.Errorf("%s resource = %v", path, prm["resource"])
		}
		if res.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: metadata must be readable cross-origin", path)
		}
	}
}

func TestRegister(t *testing.T) {
	s := apitest.New(t)

	c := s.RegisterClient(t, nil)
	if id, _ := c["client_id"].(string); !strings.HasPrefix(id, "vc_") {
		t.Errorf("client_id = %v", c["client_id"])
	}
	if c["token_endpoint_auth_method"] != "none" || c["client_secret"] != nil {
		t.Errorf("a public client gets no secret: %v", c)
	}

	conf := s.RegisterClient(t, map[string]any{"client_name": "X", "redirect_uris": []string{"http://localhost:6274/cb"},
		"token_endpoint_auth_method": "client_secret_basic"})
	if sec, _ := conf["client_secret"].(string); len(sec) < 40 {
		t.Errorf("a confidential client gets a secret: %v", conf)
	}

	for name, body := range map[string]map[string]any{
		"plain http":     {"redirect_uris": []string{"http://evil.test/cb"}},
		"fragment":       {"redirect_uris": []string{"https://a.test/cb#x"}},
		"no uris":        {"redirect_uris": []string{}},
		"implicit grant": {"redirect_uris": []string{apitest.AssistantRedirect}, "grant_types": []string{"implicit"}},
		"bad method":     {"redirect_uris": []string{apitest.AssistantRedirect}, "token_endpoint_auth_method": "private_key_jwt"},
	} {
		res := s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/oauth/register", Body: body})
		if res.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, res.Code, res.Body)
		}
	}
}

func TestFullFlowAndRefreshRotation(t *testing.T) {
	s := apitest.New(t)
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	clientID := s.RegisterClient(t, nil)["client_id"].(string)

	loc := s.Authorize(t, apitest.AuthorizeParams(clientID))
	if loc.Host != "localhost:3005" || loc.Path != "/conectar" {
		t.Fatalf("authorize sent the browser to %s, want the app's /conectar", loc)
	}
	reqID := loc.Query().Get("pedido")

	var req struct {
		ClientName   string   `json:"clientName"`
		RedirectHost string   `json:"redirectHost"`
		Scopes       []string `json:"scopes"`
	}
	s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/connection-requests/" + reqID, Token: u.Token}).Status(t, http.StatusOK).JSON(t, &req)
	if req.ClientName != "Claude" || req.RedirectHost != "assistant.test" || len(req.Scopes) != 1 || req.Scopes[0] != "finance:read" {
		t.Fatalf("consent screen data = %+v", req)
	}

	back := s.Approve(t, u, reqID, true)
	if back.Host != "assistant.test" || back.Query().Get("state") != "xyz" || back.Query().Get("iss") != "https://api.test" {
		t.Fatalf("redirect back = %s", back)
	}
	code := back.Query().Get("code")

	exchange := func(code string) *apitest.Res {
		return s.TokenForm(t, url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {apitest.AssistantRedirect},
			"client_id": {clientID}, "code_verifier": {apitest.Verifier},
		}, nil)
	}
	res := exchange(code).Status(t, http.StatusOK)
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Error("token responses must not be cached")
	}
	var tok apitest.Tokens
	res.JSON(t, &tok)
	if tok.TokenType != "Bearer" || tok.ExpiresIn != 3600 || tok.Scope != "finance:read" || tok.RefreshToken == "" {
		t.Fatalf("tokens = %+v", tok)
	}
	if text, isErr := apitest.CallTool(t, s.MCP(t, tok.AccessToken), "list_spaces", nil); isErr {
		t.Fatalf("list_spaces failed with a fresh token: %s", text)
	}

	// The request is gone once answered.
	s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/connection-requests/" + reqID, Token: u.Token}).Status(t, http.StatusNotFound)

	refresh := func(rt string) *apitest.Res {
		return s.TokenForm(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {clientID}}, nil)
	}
	var next apitest.Tokens
	refresh(tok.RefreshToken).Status(t, http.StatusOK).JSON(t, &next)
	if next.RefreshToken == tok.RefreshToken || next.AccessToken == tok.AccessToken {
		t.Fatal("refresh must rotate both tokens")
	}

	// Presenting the rotated refresh token again revokes the connection.
	assertOAuthError(t, refresh(tok.RefreshToken), "invalid_grant")
	assertOAuthError(t, refresh(next.RefreshToken), "invalid_grant")
	mcpStatus(t, s, next.AccessToken, http.StatusUnauthorized)

	var conns struct {
		Items []any `json:"items"`
	}
	s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/connections", Token: u.Token}).Status(t, http.StatusOK).JSON(t, &conns)
	if len(conns.Items) != 0 {
		t.Errorf("connections after reuse = %d, want 0", len(conns.Items))
	}
	var n int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = 'oauth.reuse_revoked'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("reuse audit events = %d (%v), want 1", n, err)
	}
}

func assertOAuthError(t *testing.T, res *apitest.Res, want string) {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	res.JSON(t, &e)
	if e.Error != want {
		t.Fatalf("error = %q, want %q (status %d, %s)", e.Error, want, res.Code, res.Body)
	}
}

func mcpStatus(t *testing.T, s *apitest.Server, token string, want int) *apitest.Res {
	t.Helper()
	h := http.Header{"Accept": {"application/json, text/event-stream"}}
	res := s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/mcp", Token: token, Header: h,
		Body: map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}})
	return res.Status(t, want)
}

func TestCodeIsSingleUseAndBoundToPKCE(t *testing.T) {
	s := apitest.New(t)
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	clientID := s.RegisterClient(t, nil)["client_id"].(string)

	code := func() string {
		loc := s.Authorize(t, apitest.AuthorizeParams(clientID))
		return s.Approve(t, u, loc.Query().Get("pedido"), true).Query().Get("code")
	}
	form := func(code, verifier, redirect string) url.Values {
		return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
			"client_id": {clientID}, "code_verifier": {verifier}}
	}

	c1 := code()
	assertOAuthError(t, s.TokenForm(t, form(c1, strings.Repeat("w", 50), apitest.AssistantRedirect), nil), "invalid_grant")

	c2 := code()
	assertOAuthError(t, s.TokenForm(t, form(c2, apitest.Verifier, "https://assistant.test/other"), nil), "invalid_grant")

	c3 := code()
	var tok apitest.Tokens
	s.TokenForm(t, form(c3, apitest.Verifier, apitest.AssistantRedirect), nil).Status(t, http.StatusOK).JSON(t, &tok)
	// A second use revokes what the first one issued.
	assertOAuthError(t, s.TokenForm(t, form(c3, apitest.Verifier, apitest.AssistantRedirect), nil), "invalid_grant")
	mcpStatus(t, s, tok.AccessToken, http.StatusUnauthorized)

	// Another client cannot use someone else's code.
	other := s.RegisterClient(t, nil)["client_id"].(string)
	c4 := code()
	f := form(c4, apitest.Verifier, apitest.AssistantRedirect)
	f.Set("client_id", other)
	assertOAuthError(t, s.TokenForm(t, f, nil), "invalid_grant")
}

func TestAuthorizeErrors(t *testing.T) {
	s := apitest.New(t)
	clientID := s.RegisterClient(t, nil)["client_id"].(string)

	// Unknown client or redirect: shown, never redirected.
	for name, change := range map[string]func(url.Values){
		"unknown client":   func(v url.Values) { v.Set("client_id", "vc_nope") },
		"foreign redirect": func(v url.Values) { v.Set("redirect_uri", "https://evil.test/cb") },
	} {
		p := apitest.AuthorizeParams(clientID)
		change(p)
		res := s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/oauth/authorize?" + p.Encode(), Client: apitest.NoRedirects})
		if res.Code != http.StatusBadRequest || res.Header.Get("Location") != "" {
			t.Errorf("%s: status %d location %q, want 400 without redirect", name, res.Code, res.Header.Get("Location"))
		}
	}

	// The rest go back to the client with an error.
	for name, tc := range map[string]struct {
		change func(url.Values)
		want   string
	}{
		"no PKCE":       {func(v url.Values) { v.Del("code_challenge") }, "invalid_request"},
		"plain PKCE":    {func(v url.Values) { v.Set("code_challenge_method", "plain") }, "invalid_request"},
		"token flow":    {func(v url.Values) { v.Set("response_type", "token") }, "unsupported_response_type"},
		"unknown scope": {func(v url.Values) { v.Set("scope", "finance:read admin") }, "invalid_scope"},
		"other target":  {func(v url.Values) { v.Set("resource", "https://other.test/mcp") }, "invalid_target"},
	} {
		p := apitest.AuthorizeParams(clientID)
		tc.change(p)
		loc := s.Authorize(t, p)
		if loc.Host != "assistant.test" || loc.Query().Get("error") != tc.want || loc.Query().Get("state") != "xyz" {
			t.Errorf("%s: redirected to %s, want error=%s", name, loc, tc.want)
		}
	}

	// The resource the client names is accepted when it is ours.
	p := apitest.AuthorizeParams(clientID)
	p.Set("resource", "https://api.test/mcp")
	if loc := s.Authorize(t, p); loc.Path != "/conectar" {
		t.Errorf("own resource: redirected to %s", loc)
	}
}

func TestDenyAndUnverified(t *testing.T) {
	s := apitest.New(t)
	clientID := s.RegisterClient(t, nil)["client_id"].(string)

	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	loc := s.Authorize(t, apitest.AuthorizeParams(clientID))
	back := s.Approve(t, u, loc.Query().Get("pedido"), false)
	if back.Query().Get("error") != "access_denied" || back.Query().Get("code") != "" {
		t.Errorf("deny redirected to %s", back)
	}

	unverified := s.Register(t)
	loc = s.Authorize(t, apitest.AuthorizeParams(clientID))
	s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/connection-requests/" + loc.Query().Get("pedido"), Token: unverified.Token,
		Body: map[string]bool{"approve": true}}).Status(t, http.StatusForbidden)

	s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/connection-requests/" + loc.Query().Get("pedido")}).Status(t, http.StatusUnauthorized)
}

func TestDisconnectAndRevoke(t *testing.T) {
	s := apitest.New(t)
	u := s.Register(t, apitest.RegisterOpts{Verify: true})

	_, tok := s.ConnectAssistant(t, u)
	var conns struct {
		Items []struct {
			ID         string  `json:"id"`
			ClientName string  `json:"clientName"`
			LastUsedAt *string `json:"lastUsedAt"`
		} `json:"items"`
	}
	s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/connections", Token: u.Token}).Status(t, http.StatusOK).JSON(t, &conns)
	if len(conns.Items) != 1 || conns.Items[0].ClientName != "Claude" {
		t.Fatalf("connections = %+v", conns.Items)
	}

	other := s.Register(t, apitest.RegisterOpts{Verify: true})
	s.Do(t, apitest.Req{Method: http.MethodDelete, Path: "/connections/" + conns.Items[0].ID, Token: other.Token}).Status(t, http.StatusNotFound)
	mcpStatus(t, s, tok.AccessToken, http.StatusOK)

	s.Do(t, apitest.Req{Method: http.MethodDelete, Path: "/connections/" + conns.Items[0].ID, Token: u.Token}).Status(t, http.StatusNoContent)
	mcpStatus(t, s, tok.AccessToken, http.StatusUnauthorized)

	// RFC 7009 from the client side.
	clientID, tok2 := s.ConnectAssistant(t, u)
	res := s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/oauth/revoke",
		Body:   url.Values{"token": {tok2.RefreshToken}, "client_id": {clientID}}.Encode(),
		Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}})
	res.Status(t, http.StatusOK)
	mcpStatus(t, s, tok2.AccessToken, http.StatusUnauthorized)
}

func TestConfidentialClient(t *testing.T) {
	s := apitest.New(t)
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	c := s.RegisterClient(t, map[string]any{"client_name": "Inspector", "redirect_uris": []string{apitest.AssistantRedirect},
		"token_endpoint_auth_method": "client_secret_basic"})
	id, secret := c["client_id"].(string), c["client_secret"].(string)

	code := func() string {
		loc := s.Authorize(t, apitest.AuthorizeParams(id))
		return s.Approve(t, u, loc.Query().Get("pedido"), true).Query().Get("code")
	}
	form := func(code string) url.Values {
		return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {apitest.AssistantRedirect}, "code_verifier": {apitest.Verifier}}
	}
	basic := func(id, secret string) http.Header {
		raw := url.QueryEscape(id) + ":" + url.QueryEscape(secret)
		return http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(raw))}}
	}

	res := s.TokenForm(t, form(code()), basic(id, "wrong"))
	res.Status(t, http.StatusUnauthorized)
	assertOAuthError(t, res, "invalid_client")

	f := form(code())
	f.Set("client_id", id)
	assertOAuthError(t, s.TokenForm(t, f, nil), "invalid_client") // no secret

	s.TokenForm(t, form(code()), basic(id, secret)).Status(t, http.StatusOK)
}

func TestTokensNeedAnActiveAccount(t *testing.T) {
	s := apitest.New(t)
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	_, tok := s.ConnectAssistant(t, u)

	if _, err := s.Pool.Exec(context.Background(), `UPDATE users SET deleted_at = now() WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	mcpStatus(t, s, tok.AccessToken, http.StatusUnauthorized)
}

func TestMCPWithoutTokenPointsToMetadata(t *testing.T) {
	s := apitest.New(t)
	res := mcpStatus(t, s, "", http.StatusUnauthorized)
	if got := res.Header.Get("WWW-Authenticate"); !strings.Contains(got, `resource_metadata="https://api.test/.well-known/oauth-protected-resource/mcp"`) {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	mcpStatus(t, s, "not-a-token", http.StatusUnauthorized)
}

func TestCleanup(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	s := apitest.New(t, apitest.WithNow(func() time.Time { return clock }))
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	s.ConnectAssistant(t, u)                                                                // a client with a connection stays
	s.RegisterClient(t, nil)                                                                // a client nobody connected goes after a day
	s.Authorize(t, apitest.AuthorizeParams(s.RegisterClient(t, nil)["client_id"].(string))) // its request expires

	if _, err := s.Pool.Exec(context.Background(), `UPDATE oauth_clients SET created_at = $1`, now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(31 * 24 * time.Hour)
	if _, err := s.Jobs.OAuthCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		var n int
		if err := s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("oauth_clients"); n != 1 {
		t.Errorf("clients left = %d, want 1 (the connected one)", n)
	}
	for _, table := range []string{"oauth_requests", "oauth_codes", "oauth_tokens"} {
		if n := count(table); n != 0 {
			t.Errorf("%s left = %d, want 0", table, n)
		}
	}
	if n := count("oauth_grants"); n != 1 {
		t.Errorf("connections left = %d, want 1", n)
	}
}
