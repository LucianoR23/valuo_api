# MCP connector (plan 8.6)

A person connects their own AI assistant (Claude, ChatGPT) to their money.
The assistant talks to our MCP server at `/mcp`; we make no AI calls.

## Pieces

| Path | What |
|---|---|
| `/.well-known/oauth-protected-resource[/mcp]` | RFC 9728: the resource (`API_PUBLIC_URL/mcp`) and its authorization server |
| `/.well-known/oauth-authorization-server` | RFC 8414 metadata |
| `POST /oauth/register` | RFC 7591 dynamic registration (20 an hour per IP). Public clients get no secret; a client that asks for `client_secret_post` or `client_secret_basic` gets one |
| `GET /oauth/authorize` | Code flow, PKCE S256 required. Keeps the request 10 minutes and sends the browser to `APP_BASE_URL/conectar?pedido=<id>` |
| `GET/POST /connection-requests/{id}` | The app's consent screen (signed in, verified email) reads and answers the request |
| `POST /oauth/token` | `authorization_code` (2 minute, single-use codes) and `refresh_token` (rotating). Access tokens last 1 hour, refresh tokens 30 days |
| `POST /oauth/revoke` | RFC 7009 |
| `GET /connections`, `DELETE /connections/{id}` | The person's connections, for `/ajustes/conexiones` |
| `POST /mcp` | Streamable HTTP, stateless, JSON responses. Bearer token with `finance:read` |

The authorization server is our own code (`internal/oauth`), not a library:
fosite was the plan's choice, but its last release is from 2024-12 and pulls
dependencies with known vulnerabilities (decided with the user on 2026-11-16).
Only `github.com/modelcontextprotocol/go-sdk` was added.

## Security

- Codes and tokens are 32 random bytes; only their SHA-256 is stored.
- A code used twice, or a rotated refresh token used again, deletes the whole
  connection and records `oauth.reuse_revoked`.
- Every token check also checks that the account exists and is verified.
- Tools call the app's own endpoints in process as the person, so space
  membership and every other rule of the app apply unchanged.
- 60 calls an hour and 500 a day per connection (`mcp_calls`). Every call is
  an `mcp.call` audit event (not shown in the person's recent activity);
  connecting and disconnecting are `oauth.connected` and `oauth.disconnected`.
- Answers over 64 KB are refused with a hint to narrow the question.
- An hourly job deletes expired requests, codes, tokens and counters, and
  clients nobody connected within a day of registering.
- CORS on `/.well-known/oauth-*`, `/oauth/*` and `/mcp` is open and without
  credentials: those routes use bearer tokens, never cookies.

## Tools (read only)

`list_spaces`, `get_month_summary`, `list_expenses`, `list_payment_methods`,
`get_committed_payments`, `get_card_statements`, `get_budgets_progress`,
`get_exchange_rates`, `simulate_purchase`. Spaces are `personal` (default) or
`hogar`. Writing drafts (`finance:write_drafts`) comes with 8.6b.

## Try it

1. `npx @modelcontextprotocol/inspector`, transport "Streamable HTTP", URL
   `http://localhost:8080/mcp`, then "Open Auth Settings" and run the flow. The
   app (`pnpm dev`) must be running for the consent screen.
2. On staging, add a custom connector in Claude with
   `https://<staging API>/mcp`.

No new variables: it uses `API_PUBLIC_URL` (must be the public https origin,
it is the issuer and the resource) and `APP_BASE_URL`.
