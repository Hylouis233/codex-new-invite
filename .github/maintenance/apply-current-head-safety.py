from pathlib import Path
import re
import subprocess

base = dst = Path.cwd()
text = (base / 'main.go').read_text()
previous = subprocess.check_output(['git', 'show', '49dca194198b29e72f176c4c3b9e2f82903e38c6:main.go'], text=True)

def body(source, name):
    start = source.index('func ' + name + '(')
    end = source.index('\n}\n', start) + 3
    return source[start:end]

def replace_func(source, name, replacement):
    old = body(source, name)
    assert source.count(old) == 1
    return source.replace(old, replacement, 1)

newparse = '''func parseQueryRequest(body []byte) (queryRequest, error) {
	if len(body) > maxManagementBodyBytes {
		return queryRequest{}, httpStatusError{status: http.StatusRequestEntityTooLarge, msg: "request body is too large"}
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return queryRequest{}, nil
	}
	if trimmed[0] != '{' {
		return queryRequest{}, httpStatusError{status: http.StatusBadRequest, msg: "JSON object required"}
	}
	var payload queryRequest
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return queryRequest{}, httpStatusError{status: http.StatusBadRequest, msg: "invalid JSON request body"}
	}
	return payload, nil
}
'''
text = replace_func(text, 'parseQueryRequest', newparse)
for name in ['handleUsage', 'handleReferrals', 'handleProbe', 'handleRedeem']:
    function = body(text, name)
    function = function.replace('payload := parseQueryRequest(req.Body)', '''payload, errPayload := parseQueryRequest(req.Body)
	if errPayload != nil {
		return jsonResponse(statusForError(errPayload), map[string]any{"error": errPayload.Error()})
	}''')
    text = replace_func(text, name, function)

for name in ['handleActivate', 'handleCDPActivate', 'handleDesktopActivate']:
    text = replace_func(text, name, f'''func {name}(_ pluginapi.ManagementRequest) pluginapi.ManagementResponse {{
	return jsonResponse(http.StatusGone, map[string]any{{"error": "local activation is not supported by this plugin"}})
}}
''')
text = text.replace('\n\t"os"', '').replace('\n\t"os/exec"', '')
for path in ['activate', 'cdp-activate', 'desktop-activate']:
    text = re.sub(r'^\s*\{Method: http.MethodPost, Path: "/codex-invite/' + path + r'"\},\n', '', text, flags=re.M)
start = text.index('// handleActivate ')
end = text.index('func handleActivate(', start)
text = text[:start] + '// Deprecated local activation routes return Gone without executing external programs.\n' + text[end:]
for name in ['handleCDPActivate', 'handleDesktopActivate']:
    start = text.index('// ' + name + ' ')
    end = text.index('func ' + name + '(', start)
    text = text[:start] + text[end:]
start = text.index('      <div class="actions" style="margin-top:8px">\n        <input id="activateEmail"')
end = text.index('      </div>', start) + len('      </div>\n')
text = text[:start] + text[end:]
start = text.index('      // Activate: one-click full chain')
end = text.index('    localeSelect.addEventListener', start)
text = text[:start] + text[end:]
text = text.replace("    activateBtn.addEventListener('click', () => guardKey(activateAccount));\n", '')
start = text.index('    // Desktop Activate:')
end = text.index('    clearBtn.addEventListener', start)
text = text[:start] + text[end:]

start = text.index('\t// defaultCDKSiteURL ')
end = text.index('\n\t// dispatchPollInterval', start)
text = text[:start] + '''\t// No third-party CDK destination is selected until the operator configures one.
	defaultCDKSiteURL = ""''' + text[end:]
start = text.index('\tcfg.CDKSiteURL = strings.TrimRight')
end = text.index('\n\treturn cfg', start)
text = text[:start] + '''\t// Invalid or absent configuration disables dispatch rather than selecting a vendor.
	cfg.CDKSiteURL, _ = normalizeCDKSiteURL(cfg.CDKSiteURL)''' + text[end:]
function = body(text, 'handleDispatch')
needle = '\tcfg := currentConfig()\n'
assert function.count(needle) == 1
function = function.replace(needle, '''\tcfg := currentConfig()
	siteURL, errSite := normalizeCDKSiteURL(cfg.CDKSiteURL)
	if errSite != nil || siteURL == "" {
		return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": "dispatch requires an explicitly configured HTTPS cdk_site_url"})
	}
	cfg.CDKSiteURL = siteURL
	if strings.TrimSpace(payload.AuthIndex) == "" && strings.TrimSpace(payload.AuthName) == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "explicit managed account required"})
	}
''')
text = replace_func(text, 'handleDispatch', function)
function = body(text, 'cdkSiteCall')
function = function.replace('\traw, errMarshal := json.Marshal(body)', '''\tvalidated, errSite := normalizeCDKSiteURL(siteURL)
	if errSite != nil || validated == "" {
		return nil, 0, fmt.Errorf("a configured HTTPS CDK origin is required")
	}
	switch path {
	case "/api/lookup", "/api/use", "/api/status":
	default:
		return nil, 0, fmt.Errorf("unsupported CDK API path")
	}
	if client == nil {
		return nil, 0, fmt.Errorf("CDK HTTP client is required")
	}
	// Do not mutate the caller's client or permit redirects to replay credential bodies.
	boundedClient := *client
	boundedClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	siteURL = validated
	raw, errMarshal := json.Marshal(body)''')
function = function.replace('resp, errDo := client.Do(req)', 'resp, errDo := boundedClient.Do(req)')
function = function.replace('''	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &decoded)
	}''', '''	if err := json.Unmarshal(payload, &decoded); err != nil || decoded == nil {
		return nil, resp.StatusCode, fmt.Errorf("invalid CDK response object")
	}''')
text = replace_func(text, 'cdkSiteCall', function)
text = replace_func(text, 'sendInvite', body(previous, 'sendInvite') + '\n' + body(previous, 'legacyInviteFallbackStatus'))
(dst / 'main.go').write_text(text)
security = (base / 'security.go').read_text()
security += '''
// dispatchRequest must enforce the same credential origin as invite/auto-assign.
func (request *dispatchRequest) UnmarshalJSON(data []byte) error {
	type wireDispatchRequest dispatchRequest
	var decoded wireDispatchRequest
	if err := json.Unmarshal(data, &decoded); err != nil { return err }
	baseURL, err := normalizeCredentialBaseURL(decoded.BaseURL)
	if err != nil { return err }
	decoded.BaseURL = baseURL
	*request = dispatchRequest(decoded)
	return nil
}

// normalizeCDKSiteURL accepts only an explicit HTTPS origin, never a URL with
// embedded credentials, an alternate path, or query/fragment URL suffixes.
// Host ownership remains the operator's trust decision; HTTPS alone does not verify it.
func normalizeCDKSiteURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" { return "", nil }
	parsed, err := url.Parse(raw)
	if err != nil { return "", fmt.Errorf("invalid CDK origin") }
	path := parsed.EscapedPath()
	if parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		(path != "" && path != "/") || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || strings.Contains(raw, "#") || parsed.Opaque != "" {
		return "", fmt.Errorf("CDK site must be an HTTPS origin without credentials, path, query or fragment")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed.String(), nil
}
'''
(dst / 'security.go').write_text(security)
readme = (base / 'README.md').read_text()
readme = readme.replace('with automatic fallback to the legacy `wham/referrals/invite` endpoint.', 'with legacy fallback only for definitive unsupported-endpoint responses (404, 405, 410, or 501). Transport failures, authentication failures, rate limits, and ambiguous server errors never cause an automatic second send.')
readme = readme.replace('`cdk_site_url`, default `https://abc.dpzxsm.qzz.io`; config-only, never request-overridable since CDKs are credentials', '`cdk_site_url`, disabled until the operator explicitly configures a trusted HTTPS origin; never request-overridable since CDKs are credentials')
insert = '''
## Current-head safety boundaries

CDK dispatch is disabled when `cdk_site_url` is absent or invalid. No third-party service is selected by default. Configure only an HTTPS origin you explicitly trust to receive CDK credentials; the plugin does not verify the site's ownership. Redirects are not followed for CDK requests. Dispatch request `base_url` is restricted to the same ChatGPT origin as other credential-bearing requests.

The former machine-specific `/activate`, `/cdp-activate`, and `/desktop-activate` hooks are retired. They are no longer advertised or shown in the dashboard; direct calls return HTTP 410 without locating or running external scripts. Query, invite, and separately configured dispatch APIs remain available.

Malformed, oversized, and non-object query bodies fail before account lookup. These changes are scoped hardening, not a blanket security approval: the transport lifecycle, management-origin trust, and redemption retry identity still require independent review. The upstream contribution remains Draft.

'''
readme = readme.replace('## Features\n', insert + '## Features\n', 1)
(dst / 'README.md').write_text(readme)
subprocess.run(['gofmt', '-w', 'security.go'], check=True)
