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

### Auto-assign invites by quota (v0.3.0)

`POST /v0/management/codex-invite/auto-assign` — input a list of invitee emails and the plugin distributes them across **every active Codex account that can still invite**, filling accounts from the **highest remaining invite quota down to the lowest**:

- Per-account remaining quota is resolved in priority order: `invite/eligibility` (`remaining_send_capacity`, exact) → `invite/tracking` (90-day sent count subtracted from an assumed capacity) → the assumed capacity itself (default 10, override with `fallback_capacity`).
- Ties are broken by credit balance; `per_account_limit` optionally caps how many emails one account receives.
- `dry_run: true` previews the plan (quota snapshot + assignment) without sending anything.
- Failed sends (expired token, quota exhausted mid-run) automatically roll the emails over to the next account with spare capacity; the response reports per-account `sent/failed` plus any `unassigned_emails`.
- `accounts_filter` restricts the pool (auth index / file name / email substring match).
- The invite page has **预览分配 / 自动分配并发送** buttons wired to this endpoint.

### Redeem reward (Codex Usage page)

- **Redeem** a banked rate-limit reset credit — `POST /v0/management/codex-invite/redeem`, which lists banked credits (`GET /backend-api/wham/rate-limit-reset-credits`) and redeems the first available one via `POST /backend-api/wham/rate-limit-reset-credits/consume` (`credit_id` + a generated `redeem_request_id`).
- This is the step that actually applies an earned referral reward — without redeeming, the credit stays banked and does not restore your rate-limit window.

## CDK dispatch (v0.4.0)

`POST /v0/management/codex-invite/dispatch` — end-to-end CDK redemption against the card site (`cdk_site_url`, default `https://abc.dpzxsm.qzz.io`; config-only, never request-overridable since CDKs are credentials): per CDK it runs **lookup → invite from the selected account → trigger auto-accept → poll status** until redemption. Non-ready CDKs are skipped (idempotent), a failed invite never consumes the CDK, and `dry_run` previews which CDKs are ready. The invite page gains a **CDK 派发** panel.

Compatibility fixes in the same release:
- An invalid `base_url` in plugin YAML now **downgrades to the default instead of failing the whole plugin load** (request-level checks stay strict).
- The auto-assign tracking estimate now only counts invites from the **last 31 days**, matching the monthly quota window (stale 90-day records no longer understate capacity).

## Security hardening (v0.3.1)

Ported from the `agent/review-fix-20260827` review branch onto the current feature set:

- **Credential origin pinning** — `base_url` on every request type and in plugin YAML must be exactly `https://chatgpt.com`; credentials can no longer be pointed at an attacker-controlled origin (proxying stays available via `proxy_url`).
- **No redirect following** — both the ChatGPT upstream client and the CPA management callback client return redirect responses as-is, so Authorization/Cookie headers never leave the intended origin.
- **Management origin allowlist** — plugin management callbacks only accept loopback, RFC1918 private, and Tailscale CGNAT (100.64/10) origins; public internet origins are rejected (keeps LAN/tailnet CPAMC deployments working).
- **Redeem requires an explicit account** — the credit-consuming endpoint no longer silently falls back to the first managed credential.
- **UI XSS fixes** — invite links are `https:`-only without embedded credentials (`safeInviteURL`), the usage page renders all dynamic metrics via `textContent` descriptors, and the management key is no longer persisted to `localStorage`.
- Registration metadata now points at this fork (Hylouis233 / codex-new-invite).

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
| **Usage page fixes** | Added management-key input, `X-Codex-Invite-Origin` header, GET→POST for fetch-body support, and account-status metrics on the referrals fallback path. |

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
3. Enter your CPA management key (saved to localStorage for convenience).
4. Select a Codex credential, then query usage / send invites.

## License

MIT
