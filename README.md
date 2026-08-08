# Codex Invite CLIProxyAPI Plugin

`codex-invite` is a CLIProxyAPI dynamic library plugin that exposes
Management UI resources for **sending Codex referral invite emails** and
**querying Codex account usage / remaining invite capacity** with an existing
Codex OAuth credential managed by CPA.

The plugin does not persist ChatGPT access tokens. At request time it reads
the selected Codex auth file through CPA's authenticated Management API,
extracts the current `access_token` and account ID, and calls:

- `POST https://chatgpt.com/backend-api/wham/referrals/invite` — send invites
- `GET https://chatgpt.com/backend-api/codex/usage` — credit balance, rate-limit usage, and referral-granted reset credits
- `GET https://chatgpt.com/backend-api/wham/referrals/status` and `/credits` — best-effort remaining-invite probe (falls back to the usage payload)

## Configuration

The plugin does not expose invite fields in the Management Center plugin
configuration form. Plugin config is only used to enable the plugin:

```yaml
plugins:
  enabled: true
  configs:
    codex-invite:
      enabled: true
      priority: 1
```

## Resource Page

The plugin resource page is available at:

```text
/v0/resource/plugins/codex-invite/invite
```

It provides:

- CPA management key entry for authenticated Management API calls.
- Codex credential loading and account selection from CPA auth files.
- Invite settings for referral key, ChatGPT base URL, language, originator, user agent, request email limit, and optional Cookie.
- A visible per-request proxy URL field in the invite form.
- Local browser settings for non-secret fields, excluding proxy URL.
- Invite execution through `POST /v0/management/codex-invite/invite`.

### Usage page

The plugin also registers a second resource page at:

```text
/v0/resource/plugins/codex-invite/usage
```

It provides read-only queries against the selected Codex credential:

- **Query usage** — calls `GET /v0/management/codex-invite/usage`, which proxies
  `GET https://chatgpt.com/backend-api/codex/usage` and surfaces:
  - `credits.balance` — current credit balance
  - `rate_limit.primary_window.used_percent` / `reset_after_seconds` — 5h usage window
  - `rate_limit.secondary_window.*` — weekly usage window (when present)
  - `rate_limit_reset_credits.available_count` / `used_count` — referral-granted reset credits
- **Query remaining invites** — calls `GET /v0/management/codex-invite/referrals`,
  which probes `GET /backend-api/wham/referrals/status` then `/credits`, falling
  back to the usage payload. ChatGPT does not expose a single dedicated
  remaining-invite counter, so the best available number plus the raw upstream
  body are both shown for transparency.

Both query endpoints accept an optional JSON body (empty body is fine for a
plain GET with a single account):

```json
{
  "auth_index": "<optional>",
  "auth_name": "<optional>",
  "management_origin": "http://127.0.0.1:8317"
}
```

The page does not store the CPA management key, proxy URL, or Cookie in `localStorage`.
Invite details and account choice are entered in this custom page, not in the
plugin configuration form.

## Build

```bash
make test
make build
make package
```

On macOS this creates:

```text
dist/codex-invite.dylib
dist/codex-invite_0.1.4_darwin_arm64.zip
dist/codex-invite_0.1.4_darwin_arm64.zip.sha256
```

Install locally by copying the dynamic library to CPA's plugin discovery
directory, for example:

```bash
mkdir -p /path/to/CLIProxyAPI/plugins/darwin/arm64
cp dist/codex-invite.dylib /path/to/CLIProxyAPI/plugins/darwin/arm64/codex-invite.dylib
```

Target platform, output directory, and runtime plugin version can be overridden:

```bash
make build GOOS=darwin GOARCH=arm64 BUILD_DIR=/path/to/plugins/darwin/arm64
make package VERSION=0.1.4
```

## Plugin Store Release

For plugin-store installation, each GitHub release must include:

```text
codex-invite_<version>_<goos>_<goarch>.zip
checksums.txt
```

Each zip must contain the dynamic library at the zip root:

- Darwin: `codex-invite.dylib`
- Linux: `codex-invite.so`
- Windows: `codex-invite.dll`

`checksums.txt` must be in sha256sum format.

Generate a local aggregate checksum file with:

```bash
make checksums VERSION=0.1.4
```

## Management API

The plugin registers:

- `GET /v0/management/codex-invite/accounts`
- `POST /v0/management/codex-invite/invite`
- `GET /v0/management/codex-invite/usage`
- `GET /v0/management/codex-invite/referrals`
- resource page `/v0/resource/plugins/codex-invite/invite`
- resource page `/v0/resource/plugins/codex-invite/usage`

The resource page asks for the CPA management key because plugin iframes are
served from the CPA backend origin and cannot read the Management Center's
frontend auth store.
