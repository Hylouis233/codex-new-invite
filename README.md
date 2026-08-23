# Codex Invite and Usage Plugin

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin for managing Codex referral invites, usage information, referral eligibility, invite tracking, and banked reset credits from the CPA management interface.

This fork extends `LTbinglingfeng/cpa-plugin-codex-invite` with a bilingual management UI, additional Codex/ChatGPT backend integrations, manual in-memory credential support, proxy support, and a Chrome-compatible TLS transport for direct `chatgpt.com` requests.

## Capabilities

### Codex Invite

- Select a Codex OAuth credential managed by CPA.
- On newer hosts, retrieve credentials through the host auth callbacks.
- On older hosts, use a compatibility fallback restricted to loopback management origins with redirects disabled.
- Optionally provide an `access_token` and `account_id` for the current request only. Manual credentials are not persisted.
- Send one or more referral invitations through the current referral endpoint, with a legacy endpoint fallback.
- Support HTTP, HTTPS, and SOCKS5 proxies.
- Render returned invitation URLs only when they are valid HTTPS URLs; invalid values remain plain text.

### Codex Usage

- Query Codex usage and rate-limit windows.
- Query referral eligibility, reward metadata, and remaining send/reward capacity.
- Query invitations sent during the available tracking window.
- List banked rate-limit reset credits.
- Redeem an available reset credit.
- Use an English or Chinese interface with locale auto-detection.

## Security model

The plugin handles bearer credentials and CPA management access, so the following boundaries are enforced:

- Credential-bearing upstream requests are pinned to the exact `https://chatgpt.com` origin. Custom schemes, hosts, ports, URL user information, paths, queries, and fragments are rejected as base URLs.
- CPA management-key values remain in the current page session and are not written to `localStorage`.
- Dynamic account, usage, referral, and error data are inserted into the UI through text nodes rather than dynamic HTML.
- Returned invitation links must use HTTPS and must not contain URL user information before being assigned to an anchor.
- The legacy management API fallback accepts only `localhost` or IP loopback origins, rejects URL user information, and does not follow redirects.
- Upstream response bodies are size-limited, and management request bodies are bounded.
- No credentials, tokens, or machine-local command hooks are included in the tracked source tree.

The host auth callbacks are opportunistic for compatibility: hosts that do not yet provide `host.auth.list` and `host.auth.get` automatically use the restricted loopback fallback.

## Management API routes

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v0/management/codex-invite/accounts` | List available Codex credentials |
| `POST` | `/v0/management/codex-invite/invite` | Send referral invitations |
| `POST` | `/v0/management/codex-invite/usage` | Query usage and rate-limit windows |
| `POST` | `/v0/management/codex-invite/referrals` | Query referral eligibility, tracking, and credits |
| `POST` | `/v0/management/codex-invite/redeem` | Redeem one available reset credit |
| `POST` | `/v0/management/codex-invite/probe` | Run read-only diagnostics against allowed `/backend-api/*` paths |

## Upstream endpoints

The plugin currently integrates with these `chatgpt.com` backend paths:

| Method | Endpoint |
| --- | --- |
| `POST` | `/backend-api/referrals/invite` |
| `POST` | `/backend-api/wham/referrals/invite` |
| `GET` | `/backend-api/codex/usage` |
| `GET` | `/backend-api/referrals/invite/eligibility` |
| `GET` | `/backend-api/referrals/invite/tracking` |
| `GET` | `/backend-api/wham/rate-limit-reset-credits` |
| `POST` | `/backend-api/wham/rate-limit-reset-credits/consume` |
| `GET` | `/backend-api/wham/usage` |

These are service-internal endpoints and may change independently of this plugin. Errors are returned to the management UI without silently treating an unexpected response as success.

## Transport behavior

Direct `chatgpt.com` requests use a uTLS `HelloChrome_Auto` ClientHello profile. Other HTTPS hosts, including proxy connections, use the standard Go transport with normal certificate verification. The transport does not disable TLS verification.

## Build

Requirements:

- Go 1.26 or newer
- A C compiler supported by CGO

Run the local checks:

```sh
make test
make vet
```

Build a Windows AMD64 shared library:

```sh
export CGO_ENABLED=1 GOOS=windows GOARCH=amd64
go build -trimpath -buildmode=c-shared \
  -ldflags "-s -w -X main.pluginVersion=0.2.0" \
  -o dist/codex-invite.dll .
```

The repository workflow also builds and packages Linux, macOS, Windows, FreeBSD, AMD64, and ARM64 targets where configured.

## Usage

1. Install the built library in the CPA plugin directory for the target operating system and architecture.
2. Restart CPA and open its management interface.
3. Open **Codex Invite** or **Codex Usage** from the plugin menu.
4. Enter the CPA management key for the current page session.
5. Select a managed Codex credential, or use the explicit manual-credential fields for a one-off request.
6. Query usage/referral status, send invitations, or redeem an available credit.

## Testing

The test suite covers:

- exact-origin validation for credential-bearing requests;
- loopback-only management fallback and redirect rejection;
- host callback request/response handling;
- dynamic UI text rendering and management-key non-persistence;
- HTTPS-only invitation navigation;
- upstream response-size limits and request validation.

## License

MIT
