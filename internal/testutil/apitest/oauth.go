package apitest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Assistant redirect URI used by the OAuth helpers (plan 8.6).
const AssistantRedirect = "https://assistant.test/callback"

// Verifier is a fixed PKCE code verifier (43+ characters).
const Verifier = "test-verifier-0123456789-abcdefghijklmnopqrstuvwxyz"

// Challenge returns the S256 code challenge of a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// NoRedirects is a client that returns redirects instead of following them.
var NoRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// RegisterClient registers an assistant (RFC 7591) and returns its client_id.
func (s *Server) RegisterClient(t testing.TB, body map[string]any) map[string]any {
	t.Helper()
	if body == nil {
		body = map[string]any{"client_name": "Claude", "redirect_uris": []string{AssistantRedirect}}
	}
	res := s.Do(t, Req{Method: http.MethodPost, Path: "/oauth/register", Body: body})
	res.Status(t, http.StatusCreated)
	var out map[string]any
	res.JSON(t, &out)
	return out
}

// Authorize starts an authorization with PKCE and returns the Location the
// browser is sent to.
func (s *Server) Authorize(t testing.TB, params url.Values) *url.URL {
	t.Helper()
	res := s.Do(t, Req{Method: http.MethodGet, Path: "/oauth/authorize?" + params.Encode(), Client: NoRedirects})
	res.Status(t, http.StatusFound)
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatalf("apitest: location: %v", err)
	}
	return loc
}

// AuthorizeParams are valid authorization parameters for clientID.
func AuthorizeParams(clientID string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {AssistantRedirect},
		"code_challenge": {Challenge(Verifier)}, "code_challenge_method": {"S256"},
		"state": {"xyz"}, "scope": {"finance:read"},
	}
}

// Approve answers a pending request as u and returns where the app sends
// the browser back.
func (s *Server) Approve(t testing.TB, u User, requestID string, approve bool) *url.URL {
	t.Helper()
	res := s.Do(t, Req{Method: http.MethodPost, Path: "/connection-requests/" + requestID, Token: u.Token,
		Body: map[string]bool{"approve": approve}})
	res.Status(t, http.StatusOK)
	var out struct {
		RedirectTo string `json:"redirectTo"`
	}
	res.JSON(t, &out)
	loc, err := url.Parse(out.RedirectTo)
	if err != nil {
		t.Fatalf("apitest: redirectTo: %v", err)
	}
	return loc
}

// TokenForm posts to /oauth/token.
func (s *Server) TokenForm(t testing.TB, form url.Values, header http.Header) *Res {
	t.Helper()
	h := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	for k, v := range header {
		h[k] = v
	}
	return s.Do(t, Req{Method: http.MethodPost, Path: "/oauth/token", Body: form.Encode(), Header: h})
}

// Tokens is a token endpoint answer.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// ConnectAssistant runs the whole flow for u (register, authorize, approve,
// exchange) and returns the client_id and tokens.
func (s *Server) ConnectAssistant(t testing.TB, u User) (string, Tokens) {
	t.Helper()
	clientID := s.RegisterClient(t, nil)["client_id"].(string)
	loc := s.Authorize(t, AuthorizeParams(clientID))
	back := s.Approve(t, u, loc.Query().Get("pedido"), true)
	res := s.TokenForm(t, url.Values{
		"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")},
		"redirect_uri": {AssistantRedirect}, "client_id": {clientID}, "code_verifier": {Verifier},
	}, nil)
	res.Status(t, http.StatusOK)
	var tok Tokens
	res.JSON(t, &tok)
	return clientID, tok
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// MCP connects an MCP client to /mcp with an access token.
func (s *Server) MCP(t testing.TB, accessToken string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "apitest", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   s.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token: accessToken, base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatalf("apitest: mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// CallTool calls a tool and returns its text and whether it is a tool error.
func CallTool(t testing.TB, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("apitest: call %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

// ToolJSON calls a tool, fails on a tool error and decodes its answer.
func ToolJSON(t testing.TB, session *mcp.ClientSession, name string, args map[string]any, v any) {
	t.Helper()
	text, isErr := CallTool(t, session, name, args)
	if isErr {
		t.Fatalf("apitest: %s returned an error: %s", name, text)
	}
	if err := json.Unmarshal([]byte(text), v); err != nil {
		t.Fatalf("apitest: decode %s: %v (%s)", name, err, text)
	}
}
