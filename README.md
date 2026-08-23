# codex-new-invite

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that sends Codex referral invite emails **and** queries Codex account usage / invite reward status, using a selected Codex OAuth credential managed by CPA.

Fork of [LTbinglingfeng/cpa-plugin-codex-invite](https://github.com/LTbinglingfeng/cpa-plugin-codex-invite) with major additions: Codex usage query, remaining-invite reward query (reverse-engineered from the Codex desktop app), uTLS Cloudflare bypass, bilingual UI (English / 中文), and the new-v2 referral invite endpoint.

## Features

### Send invites (Codex Invite page)

- Pick a Codex credential from your CPA auth files **or manually enter an `access_token` / `account_id`** (for credentials not managed by CPA). Manual credentials are used in-memory only and never persisted.
- Uses the **new referral invite endpoint** (`POST /backend-api/referrals/invite` with `program_id` / `entrypoint` / `emails`) reverse-engineered from the Codex desktop app, with automatic fallback to the legacy `wham/referrals/invite` endpoint.
- Supports proxy (HTTP / HTTPS / SOCKS5).

### Query usage (Codex Usage page)

- **Credit balance**, rate-limit usage (primary + weekly windows), and referral-granted reset credits — from `GET /backend-api/codex/usage`.
- **Remaining invite reward** — from `GET /backend-api/referrals/invite/eligibility?program_id=...&entrypoint=...`, showing:
  - Reward title & description (e.g. "获得 250/500 额度")
  - Per-side grant amounts (referrer + recipient)
  - Remaining send capacity (how many invites you can still send this month)
  - Remaining reward capacity (how many rewards you can still earn this month)
  - Eligibility rules
- **Invite tracking** — how many invites sent in the past 90 days, from `GET /backend-api/referrals/invite/tracking`.
- Bilingual UI with locale auto-detection (English / 中文), persisted to localStorage.

### Redeem reward (Codex Usage page)

- **Redeem** a banked rate-limit reset credit — `POST /v0/management/codex-invite/redeem`, which lists banked credits (`GET /backend-api/wham/rate-limit-reset-credits`) and redeems the first available one via `POST /backend-api/wham/rate-limit-reset-credits/consume` (`credit_id` + a generated `redeem_request_id`).
- This is the step that actually applies an earned referral reward — without redeeming, the credit stays banked and does not restore your rate-limit window.

## What's new in this fork (v0.2.0)

| Change | Why |
|--------|-----|
| **uTLS Chrome fingerprint** for all upstream ChatGPT requests | Go's default TLS fingerprint is blocked by Cloudflare. The plugin now uses `HelloChrome_Auto` (same as the CPA host process), so usage/referral/invite queries pass Cloudflare **without any browser Cookie**. |
| **New referral invite endpoint** (`/backend-api/referrals/invite`) | The desktop app uses a new endpoint with `program_id`/`entrypoint` body shape. The plugin tries the new endpoint first, falling back to legacy. |
| **Invite eligibility + tracking queries** | Reverse-engineered from the Codex desktop app's `app.asar`. Shows remaining invite/reward capacity, reward amounts, rules — the same data the desktop UI shows. |
| **Banked reset-credits query** (`/wham/rate-limit-reset-credits`) | Shows how many referral-granted reset credits an account has earned and has available. |
| **Redeem reward** (`POST /codex-invite/redeem` → `/wham/rate-limit-reset-credits/consume`) | Actually applies a banked referral reward to restore your rate-limit window. |
| **Manual credential mode** | Invite with an `access_token`/`account_id` you enter directly, for accounts not managed by CPA. In-memory only, never persisted. |
| **Bilingual UI (EN / 中文)** | Both the Invite and Usage pages support English and Chinese, with locale auto-detection and persistence. |
| **Usage page fixes** | Added a non-persistent management-key input, host auth callbacks for credential lookup, safe text-node rendering for dynamic metrics, GET→POST for fetch-body support, and account-status metrics on the referrals fallback path. Legacy self-calls are restricted to loopback and never follow redirects. |

## How the Cloudflare bypass works

ChatGPT's Cloudflare WAF blocks requests whose TLS ClientHello fingerprint doesn't match a real browser. Go's standard `net/http` produces a recognizable Go fingerprint → 403. The CPA host process solves this with [uTLS](https://github.com/refraction-networking/utls) (`HelloChrome_Auto`).

This plugin ships its own uTLS round tripper (mirroring the host's `utlsRoundTripper`) so the plugin's direct requests to `chatgpt.com` carry a Chrome TLS fingerprint and pass Cloudflare — **no Cookie required**.

## Endpoints

### Management API routes

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/v0/management/codex-invite/accounts` | List available Codex credentials |
| POST | `/v0/management/codex-invite/invite` | Send referral invite emails |
| POST | `/v0/management/codex-invite/usage` | Query credit balance + rate-limit usage |
| POST | `/v0/management/codex-invite/referrals` | Query invite eligibility + tracking + remaining capacity |
| POST | `/v0/management/codex-invite/redeem` | Redeem one banked rate-limit reset credit |
| POST | `/v0/management/codex-invite/probe` | Diagnostic: probe arbitrary `/backend-api/*` paths |

### Upstream ChatGPT endpoints used

| Method | Endpoint | Source |
|--------|----------|--------|
| POST | `/backend-api/referrals/invite` | Codex desktop app (new) |
| POST | `/backend-api/wham/referrals/invite` | Legacy fallback |
| GET | `/backend-api/codex/usage` | Codex CLI / VS Code extension |
| GET | `/backend-api/referrals/invite/eligibility` | Codex desktop app (`app.asar`) |
| GET | `/backend-api/referrals/invite/tracking` | Codex desktop app (`app.asar`) |
| GET | `/backend-api/wham/rate-limit-reset-credits` | VS Code extension webview bundle |
| POST | `/backend-api/wham/rate-limit-reset-credits/consume` | Redeem a banked reset credit (`credit_id`, `redeem_request_id`) |
| GET | `/backend-api/wham/usage` | Codex CLI |

## Build

Requires Go 1.26+ and a C compiler (CGO is enabled for the c-shared DLL).

```sh
export CGO_ENABLED=1 GOOS=windows GOARCH=amd64
go build -trimpath -buildmode=c-shared \
  -ldflags "-s -w -X main.pluginVersion=0.2.0" \
  -o dist/codex-invite.dll .
```

Deploy the built DLL to your CPA `plugins/<os>/<arch>/` directory (e.g. `plugins/windows/amd64/codex-invite-v0.2.0.dll`) and restart CPA.

## Usage

1. Open the CPA management center (`http://127.0.0.1:<port>/management.html`).
2. Open **Codex Invite** or **Codex Usage** from the plugin menu.
3. Enter your CPA management key. It remains only in the current page session and is not persisted to localStorage. Credential lookup uses the host's internal auth callback when available; the compatibility fallback accepts only localhost/loopback management origins and rejects redirects.
4. Select a Codex credential, then query usage / send invites.

## License

MIT
