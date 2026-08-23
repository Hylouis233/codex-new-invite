from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected one match, found {count}")
    return text.replace(old, new, 1)


path = Path("main.go")
text = path.read_text(encoding="utf-8")

text = replace_once(
    text,
    '''typedef struct {
\tuint32_t abi_version;
\tvoid* host_ctx;
\tvoid* call;
\tvoid* free_buffer;
} cliproxy_host_api;
''',
    '''typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
\tuint32_t abi_version;
\tvoid* host_ctx;
\tcliproxy_host_call_fn call;
\tcliproxy_host_free_fn free_buffer;
} cliproxy_host_api;
''',
    "typed host API",
)
text = replace_once(
    text,
    '''extern void cliproxyPluginShutdown(void);
*/
''',
    '''extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
\tstored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
\tif (stored_host == NULL || stored_host->call == NULL) {
\t\treturn 1;
\t}
\treturn stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
\tif (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
\t\tstored_host->free_buffer(ptr, len);
\t}
}
*/
''',
    "host API wrappers",
)
text = replace_once(
    text,
    '''\trequestManagementOrigin       = "X-Codex-Invite-Origin"
''',
    '''\trequestManagementOrigin       = "X-Codex-Invite-Origin"
\thostAuthListMethod             = "host.auth.list"
\thostAuthGetMethod              = "host.auth.get"
''',
    "host callback method constants",
)
text = replace_once(
    text,
    '''func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
\tif plugin == nil {
\t\treturn 1
\t}
\tplugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
''',
    '''func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
\tif plugin == nil {
\t\treturn 1
\t}
\tC.store_host_api(host)
\tplugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
''',
    "store host API at initialization",
)
text = replace_once(
    text,
    '''//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod''',
    '''//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
\tC.store_host_api(nil)
}

func callHost(method string, payload any) (json.RawMessage, error) {
\trawPayload, errMarshal := json.Marshal(payload)
\tif errMarshal != nil {
\t\treturn nil, fmt.Errorf("marshal host callback payload %s: %w", method, errMarshal)
\t}
\tcMethod := C.CString(method)
\tdefer C.free(unsafe.Pointer(cMethod))

\tvar response C.cliproxy_buffer
\tvar requestPtr *C.uint8_t
\tif len(rawPayload) > 0 {
\t\tcPayload := C.CBytes(rawPayload)
\t\tif cPayload == nil {
\t\t\treturn nil, fmt.Errorf("allocate host callback payload %s", method)
\t\t}
\t\tdefer C.free(cPayload)
\t\trequestPtr = (*C.uint8_t)(cPayload)
\t}
\tcallCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
\tvar rawResponse []byte
\tif response.ptr != nil && response.len > 0 {
\t\trawResponse = C.GoBytes(response.ptr, C.int(response.len))
\t}
\tif response.ptr != nil {
\t\tC.free_host_buffer(response.ptr, response.len)
\t}
\tif len(rawResponse) == 0 {
\t\treturn nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
\t}

\tvar env envelope
\tif errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
\t\treturn nil, fmt.Errorf("decode host callback envelope %s: %w", method, errUnmarshal)
\t}
\tif !env.OK {
\t\tif env.Error != nil {
\t\t\treturn nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
\t\t}
\t\treturn nil, fmt.Errorf("host callback %s failed", method)
\t}
\tif callCode != 0 {
\t\treturn nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
\t}
\treturn append(json.RawMessage(nil), env.Result...), nil
}

func handleMethod''',
    "Go host callback bridge",
)

start = text.find("func fetchCodexAccounts(")
end = text.find("func fetchCodexCredential(", start)
if start < 0 or end < 0:
    raise SystemExit("could not locate fetchCodexAccounts block")
accounts_block = r'''func fetchCodexAccounts(req pluginapi.ManagementRequest, explicitOrigin string) ([]accountInfo, error) {
	if raw, errHost := callHost(hostAuthListMethod, map[string]any{}); errHost == nil {
		return parseCodexAccounts(raw)
	}
	return fetchCodexAccountsViaManagement(req, explicitOrigin)
}

func fetchCodexAccountsViaManagement(req pluginapi.ManagementRequest, explicitOrigin string) ([]accountInfo, error) {
	origin, errOrigin := resolveManagementOrigin(req, explicitOrigin)
	if errOrigin != nil {
		return nil, errOrigin
	}
	authHeader := strings.TrimSpace(req.Headers.Get("Authorization"))
	if authHeader == "" {
		return nil, httpStatusError{status: http.StatusUnauthorized, msg: "CPA management key is required"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, raw, errFetch := callLocalManagement(ctx, origin, http.MethodGet, authFilesPath, authHeader, nil)
	if errFetch != nil {
		return nil, errFetch
	}
	if status != http.StatusOK {
		return nil, httpStatusError{status: http.StatusBadGateway, msg: fmt.Sprintf("failed to list CPA auth files: status %d", status)}
	}
	return parseCodexAccounts(raw)
}

func parseCodexAccounts(raw []byte) ([]accountInfo, error) {
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(raw, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode auth files response: %w", errDecode)
	}

	accounts := make([]accountInfo, 0)
	for _, file := range payload.Files {
		provider := firstString(file, "provider", "type")
		if !strings.EqualFold(provider, "codex") {
			continue
		}
		if boolValue(file["disabled"]) || boolValue(file["unavailable"]) {
			continue
		}
		name := firstString(file, "name")
		if name == "" || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		accounts = append(accounts, accountInfo{
			AuthIndex:        firstString(file, "auth_index", "auth-index"),
			Name:             name,
			Label:            firstString(file, "label"),
			Email:            firstString(file, "email"),
			Account:          firstString(file, "account"),
			ChatGPTAccountID: nestedString(file, "id_token", "chatgpt_account_id"),
			Status:           firstString(file, "status"),
			Source:           firstString(file, "source"),
		})
	}
	sort.Slice(accounts, func(i, j int) bool {
		left := strings.ToLower(accounts[i].Email + accounts[i].Name)
		right := strings.ToLower(accounts[j].Email + accounts[j].Name)
		return left < right
	})
	return accounts, nil
}

'''
text = text[:start] + accounts_block + text[end:]

start = text.find("func fetchCodexCredential(")
end = text.find("func resolveManagementOrigin(", start)
if start < 0 or end < 0:
    raise SystemExit("could not locate fetchCodexCredential block")
credential_block = r'''func fetchCodexCredential(req pluginapi.ManagementRequest, explicitOrigin string, account accountInfo) (codexCredential, error) {
	if strings.TrimSpace(account.AuthIndex) != "" {
		raw, errHost := callHost(hostAuthGetMethod, map[string]string{"auth_index": account.AuthIndex})
		if errHost == nil {
			var result struct {
				JSON json.RawMessage `json:"json"`
			}
			if errDecode := json.Unmarshal(raw, &result); errDecode != nil {
				return codexCredential{}, fmt.Errorf("decode host auth get response: %w", errDecode)
			}
			credential, errCredential := parseCodexCredential(result.JSON)
			if errCredential != nil {
				return codexCredential{}, errCredential
			}
			if credential.AccessToken == "" {
				return codexCredential{}, httpStatusError{status: http.StatusBadRequest, msg: "selected Codex credential does not contain access_token"}
			}
			return credential, nil
		}
	}
	return fetchCodexCredentialViaManagement(req, explicitOrigin, account)
}

func fetchCodexCredentialViaManagement(req pluginapi.ManagementRequest, explicitOrigin string, account accountInfo) (codexCredential, error) {
	origin, errOrigin := resolveManagementOrigin(req, explicitOrigin)
	if errOrigin != nil {
		return codexCredential{}, errOrigin
	}
	authHeader := strings.TrimSpace(req.Headers.Get("Authorization"))
	if authHeader == "" {
		return codexCredential{}, httpStatusError{status: http.StatusUnauthorized, msg: "CPA management key is required"}
	}

	path := authFileDownloadPath + "?name=" + url.QueryEscape(account.Name)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, raw, errFetch := callLocalManagement(ctx, origin, http.MethodGet, path, authHeader, nil)
	if errFetch != nil {
		return codexCredential{}, errFetch
	}
	if status != http.StatusOK {
		return codexCredential{}, httpStatusError{status: http.StatusBadGateway, msg: fmt.Sprintf("failed to download selected Codex credential: status %d", status)}
	}

	credential, errCredential := parseCodexCredential(raw)
	if errCredential != nil {
		return codexCredential{}, errCredential
	}
	if credential.AccessToken == "" {
		return codexCredential{}, httpStatusError{status: http.StatusBadRequest, msg: "selected Codex credential does not contain access_token"}
	}
	return credential, nil
}

'''
text = text[:start] + credential_block + text[end:]

start = text.find("func normalizeOrigin(")
end = text.find("func callLocalManagement(", start)
if start < 0 or end < 0:
    raise SystemExit("could not locate normalizeOrigin block")
origin_block = r'''func normalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil {
		return "", errParse
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported origin scheme")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("management origin userinfo is not allowed")
	}
	hostname := strings.TrimSpace(parsed.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("origin host is required")
	}
	isLoopback := strings.EqualFold(hostname, "localhost")
	if ip := net.ParseIP(hostname); ip != nil {
		isLoopback = ip.IsLoopback()
	}
	if !isLoopback {
		return "", fmt.Errorf("management origin must use localhost or a loopback IP")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

'''
text = text[:start] + origin_block + text[end:]

text = replace_once(
    text,
    '''\tresp, errDo := http.DefaultClient.Do(req)
''',
    '''\tclient := &http.Client{
\t\tCheckRedirect: func(_ *http.Request, _ []*http.Request) error {
\t\t\treturn http.ErrUseLastResponse
\t\t},
\t}
\tresp, errDo := client.Do(req)
''',
    "management redirect policy",
)

forbidden = (
    "void* call;",
    "void* free_buffer;",
    "func cliproxy_plugin_init(_ *C.cliproxy_host_api",
    "resp, errDo := http.DefaultClient.Do(req)",
)
remaining = [marker for marker in forbidden if marker in text]
if remaining:
    raise SystemExit(f"host-callback security markers remain: {remaining}")
for required in (
    "C.store_host_api(host)",
    "hostAuthListMethod",
    "hostAuthGetMethod",
    "management origin must use localhost or a loopback IP",
    "http.ErrUseLastResponse",
):
    if required not in text:
        raise SystemExit(f"required host-callback security marker missing: {required}")

path.write_text(text, encoding="utf-8", newline="\n")

Path("host_security_test.go").write_text(
    r'''package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNormalizeOriginAllowsOnlyLoopback(t *testing.T) {
	t.Parallel()
	valid := map[string]string{
		"http://localhost:8317/path?x=1#fragment": "http://localhost:8317",
		"http://127.0.0.1:8317":                  "http://127.0.0.1:8317",
		"http://127.42.0.9:8317":                 "http://127.42.0.9:8317",
		"https://[::1]:8317/management":          "https://[::1]:8317",
	}
	for raw, want := range valid {
		raw, want := raw, want
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeOrigin(raw)
			if err != nil {
				t.Fatalf("normalizeOrigin(%q) returned error: %v", raw, err)
			}
			if got != want {
				t.Fatalf("normalizeOrigin(%q) = %q, want %q", raw, got, want)
			}
		})
	}

	invalid := []string{
		"https://evil.example",
		"http://10.0.0.5:8317",
		"http://192.168.1.5:8317",
		"http://0.0.0.0:8317",
		"http://localhost.evil.example:8317",
		"http://user:pass@localhost:8317",
		"file:///tmp/cpa",
	}
	for _, raw := range invalid {
		raw := raw
		t.Run("reject "+raw, func(t *testing.T) {
			t.Parallel()
			if _, err := normalizeOrigin(raw); err == nil {
				t.Fatalf("normalizeOrigin(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestCallLocalManagementDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	status, _, err := callLocalManagement(context.Background(), source.URL, http.MethodGet, "/auth", "Bearer secret", nil)
	if err != nil {
		t.Fatalf("callLocalManagement returned error: %v", err)
	}
	if status != http.StatusFound {
		t.Fatalf("status = %d, want %d", status, http.StatusFound)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target was contacted %d times", got)
	}
}

func TestParseCodexAccountsAcceptsHostCallbackShape(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"files":[{"provider":"codex","auth_index":"idx-1","name":"codex-a.json","email":"a@example.com"},{"provider":"gemini","auth_index":"idx-2","name":"gemini.json"},{"provider":"codex","auth_index":"idx-3","name":"disabled.json","disabled":true}]}`)
	accounts, err := parseCodexAccounts(raw)
	if err != nil {
		t.Fatalf("parseCodexAccounts returned error: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("len(accounts) = %d, want 1", len(accounts))
	}
	if accounts[0].AuthIndex != "idx-1" || accounts[0].Name != "codex-a.json" || accounts[0].Email != "a@example.com" {
		t.Fatalf("unexpected account: %+v", accounts[0])
	}
}

func TestHostCallbackBridgeIsPresent(t *testing.T) {
	t.Parallel()
	page := renderUsagePage(defaultConfig())
	if strings.Contains(page, "management_origin must use arbitrary host") {
		t.Fatal("unexpected unsafe management-origin marker")
	}
}
''',
    encoding="utf-8",
    newline="\n",
)

readme_path = Path("README.md")
readme = readme_path.read_text(encoding="utf-8")
readme = replace_once(
    readme,
    "| **Usage page fixes** | Added a non-persistent management-key input, `X-Codex-Invite-Origin` header, safe text-node rendering for dynamic metrics, GET→POST for fetch-body support, and account-status metrics on the referrals fallback path. |",
    "| **Usage page fixes** | Added a non-persistent management-key input, host auth callbacks for credential lookup, safe text-node rendering for dynamic metrics, GET→POST for fetch-body support, and account-status metrics on the referrals fallback path. Legacy self-calls are restricted to loopback and never follow redirects. |",
    "README host callback security",
)
readme = replace_once(
    readme,
    "3. Enter your CPA management key. It remains only in the current page session and is not persisted to localStorage.",
    "3. Enter your CPA management key. It remains only in the current page session and is not persisted to localStorage. Credential lookup uses the host's internal auth callback when available; the compatibility fallback accepts only localhost/loopback management origins and rejects redirects.",
    "README management credential lookup",
)
readme_path.write_text(readme, encoding="utf-8", newline="\n")
