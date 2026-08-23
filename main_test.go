package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCollectEmailsSplitsDedupesAndValidates(t *testing.T) {
	emails, err := collectEmails(inviteRequest{
		Emails:     []string{"User@example.com,second@example.com"},
		EmailsText: "user@example.com\nthird@example.com",
	}, 10)
	if err != nil {
		t.Fatalf("collectEmails() error = %v", err)
	}
	want := []string{"User@example.com", "second@example.com", "third@example.com"}
	if !reflect.DeepEqual(emails, want) {
		t.Fatalf("emails = %#v, want %#v", emails, want)
	}
}

func TestCollectEmailsRejectsInvalidAndTooMany(t *testing.T) {
	if _, err := collectEmails(inviteRequest{EmailsText: "not-an-email"}, 10); err == nil {
		t.Fatal("collectEmails() error = nil, want invalid email error")
	}
	if _, err := collectEmails(inviteRequest{EmailsText: "a@example.com b@example.com"}, 1); err == nil {
		t.Fatal("collectEmails() error = nil, want max email error")
	}
}

func TestNormalizeOrigin(t *testing.T) {
	got, err := normalizeOrigin("https://127.0.0.1:8317/some/path?x=1")
	if err != nil {
		t.Fatalf("normalizeOrigin() error = %v", err)
	}
	if got != "https://127.0.0.1:8317" {
		t.Fatalf("origin = %q, want https://127.0.0.1:8317", got)
	}
}

func TestInviteEndpoint(t *testing.T) {
	got, err := inviteEndpoint("https://chatgpt.com/")
	if err != nil {
		t.Fatalf("inviteEndpoint() error = %v", err)
	}
	want := "https://chatgpt.com/backend-api/wham/referrals/invite"
	if got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}
}

func TestInviteHTTPClientRejectsInvalidProxyURL(t *testing.T) {
	for _, proxyURL := range []string{"ftp://127.0.0.1:7890", "http://"} {
		if _, err := inviteHTTPClient(proxyURL); err == nil {
			t.Fatalf("inviteHTTPClient(%q) error = nil, want error", proxyURL)
		}
	}
}

func TestSendInviteUsesConfiguredProxy(t *testing.T) {
	type seenRequest struct {
		Method        string
		URL           string
		Authorization string
		ContentType   string
		Body          string
	}
	seen := make(chan seenRequest, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ := io.ReadAll(r.Body)
		seen <- seenRequest{
			Method:        r.Method,
			URL:           r.URL.String(),
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			Body:          string(rawBody),
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-oai-request-id", "req-proxy-1")
		_, _ = w.Write([]byte(`{"invites":[{"email":"user@example.com","invite_url":"https://chatgpt.com/invite/abc"}]}`))
	}))
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := sendInvite(ctx,
		pluginConfig{
			BaseURL:    "http://chatgpt.example",
			Language:   "zh-CN",
			Originator: "Codex Desktop",
			UserAgent:  "test-agent",
		},
		codexCredential{AccessToken: "access-1", AccountID: "account-1"},
		accountInfo{Email: "account@example.com"},
		[]string{"user@example.com"},
		"ref-key",
		"",
		proxy.URL,
	)
	if err != nil {
		t.Fatalf("sendInvite() error = %v", err)
	}
	if !result.OK || result.RequestID != "req-proxy-1" || len(result.Invites) != 1 {
		t.Fatalf("result = %#v", result)
	}

	select {
	case req := <-seen:
		if req.Method != http.MethodPost {
			t.Fatalf("proxied method = %q, want POST", req.Method)
		}
		// sendInvite tries the new V2 endpoint first (/backend-api/referrals/invite).
		wantURL := "http://chatgpt.example/backend-api/referrals/invite"
		if req.URL != wantURL {
			t.Fatalf("proxied URL = %q, want %q", req.URL, wantURL)
		}
		if req.Authorization != "Bearer access-1" {
			t.Fatalf("authorization = %q", req.Authorization)
		}
		if req.ContentType != "application/json" {
			t.Fatalf("content type = %q", req.ContentType)
		}
		if !strings.Contains(req.Body, `"program_id":"codex_referral_consumer"`) || !strings.Contains(req.Body, `"entrypoint":"persistent"`) || !strings.Contains(req.Body, `"emails":["user@example.com"]`) {
			t.Fatalf("body = %q", req.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not receive invite request")
	}
}

func TestRenderInvitePageDoesNotPersistProxyURL(t *testing.T) {
	page := renderInvitePage(defaultConfig())
	if !strings.Contains(page, `proxy_url: field('proxyUrl').value.trim()`) {
		t.Fatalf("page does not send proxy URL from the form")
	}
	if strings.Contains(page, `proxyURL`) {
		t.Fatalf("page contains persistent proxyURL storage/default wiring")
	}
}

func TestParseCodexCredential(t *testing.T) {
	credential, err := parseCodexCredential([]byte(`{
		"type": "codex",
		"access_token": "access-1",
		"account_id": "account-1",
		"email": "user@example.com"
	}`))
	if err != nil {
		t.Fatalf("parseCodexCredential() error = %v", err)
	}
	if credential.AccessToken != "access-1" || credential.AccountID != "account-1" || credential.Email != "user@example.com" {
		t.Fatalf("credential = %#v", credential)
	}
}

func TestParseCodexCredentialTokenDataFallback(t *testing.T) {
	credential, err := parseCodexCredential([]byte(`{
		"token_data": {
			"access_token": "access-2",
			"account_id": "account-2",
			"email": "fallback@example.com"
		}
	}`))
	if err != nil {
		t.Fatalf("parseCodexCredential() error = %v", err)
	}
	if credential.AccessToken != "access-2" || credential.AccountID != "account-2" || credential.Email != "fallback@example.com" {
		t.Fatalf("credential = %#v", credential)
	}
}

func TestRenderInvitePageEscapesDefaults(t *testing.T) {
	cfg := defaultConfig()
	cfg.ReferralKey = `</script><img src=x onerror=alert(1)>`

	page := renderInvitePage(cfg)
	if strings.Contains(page, cfg.ReferralKey) {
		t.Fatalf("page contains unescaped referral key")
	}
	if !strings.Contains(page, `\u003c/script\u003e`) {
		t.Fatalf("page does not contain JSON-escaped referral key")
	}
}

func TestRenderInvitePageCollapsesSettingsAndIncludesI18n(t *testing.T) {
	page := renderInvitePage(defaultConfig())
	if !strings.Contains(page, `<details class="panel collapsible" id="settingsPanel">`) {
		t.Fatalf("page does not render Settings as a collapsed details card")
	}
	if strings.Contains(page, `<details class="panel collapsible" id="settingsPanel" open>`) {
		t.Fatalf("settings details card is open by default")
	}
	proxyInput := `<input id="proxyUrl" spellcheck="false" placeholder="http://127.0.0.1:7890">`
	inviteStart := strings.Index(page, `<h2 data-i18n="invite.title">Invite</h2>`)
	proxyIndex := strings.Index(page, proxyInput)
	settingsEnd := strings.Index(page, `</details>`)
	if proxyIndex == -1 {
		t.Fatalf("page is missing visible proxy URL input")
	}
	if inviteStart == -1 || proxyIndex < inviteStart {
		t.Fatalf("proxy URL input is not in the visible Invite panel")
	}
	if settingsEnd != -1 && proxyIndex < settingsEnd {
		t.Fatalf("proxy URL input is still inside the collapsed Settings panel")
	}
	for _, want := range []string{
		`id="localeSelect"`,
		`data-i18n="settings.title"`,
		`'settings.title': 'Settings'`,
		`'settings.title': '设置'`,
		`'invite.proxyUrl': 'Proxy URL'`,
		`'invite.proxyUrl': '代理地址'`,
		`'invite.send': 'Send invites'`,
		`'invite.send': '发送邀请'`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page is missing %q", want)
		}
	}
}

func TestRegistrationUsesCustomPageInsteadOfConfigFields(t *testing.T) {
	reg := pluginRegistration()
	if len(reg.Metadata.ConfigFields) != 0 {
		t.Fatalf("config fields = %#v, want none", reg.Metadata.ConfigFields)
	}

	raw, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatalf("handleMethod(MethodManagementRegister) error = %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope ok = false, error = %#v", env.Error)
	}

	var registration managementRegistrationResponse
	if err := json.Unmarshal(env.Result, &registration); err != nil {
		t.Fatalf("decode management registration: %v", err)
	}
	if len(registration.Resources) != 2 {
		t.Fatalf("resources = %#v, want two custom pages (invite + usage)", registration.Resources)
	}
	resByPath := map[string]pluginapi.ResourceRoute{}
	for _, res := range registration.Resources {
		resByPath[res.Path] = res
	}
	if got, ok := resByPath["/invite"]; !ok || got.Menu != "Codex Invite" {
		t.Fatalf("invite resource = %#v, want /invite Codex Invite", resByPath["/invite"])
	}
	if got, ok := resByPath["/usage"]; !ok || got.Menu != "Codex Usage" {
		t.Fatalf("usage resource = %#v, want /usage Codex Usage", resByPath["/usage"])
	}

	routes := map[string]bool{}
	for _, route := range registration.Routes {
		routes[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		http.MethodGet + " /codex-invite/accounts",
		http.MethodPost + " /codex-invite/invite",
		http.MethodPost + " /codex-invite/usage",
		http.MethodPost + " /codex-invite/referrals",
		http.MethodPost + " /codex-invite/probe",
		http.MethodPost + " /codex-invite/redeem",
	} {
		if !routes[want] {
			t.Fatalf("registered routes = %#v, missing %s", registration.Routes, want)
		}
	}
}

func TestResolveManualCredential(t *testing.T) {
	// Empty access_token -> manual mode off.
	if _, _, manual := resolveManualCredential("", "acct-1", "a@example.com"); manual {
		t.Fatalf("manual mode should be off when access_token is empty")
	}
	// Present access_token -> manual mode on, credential populated.
	cred, acc, manual := resolveManualCredential("token-abc", "acct-1", "a@example.com")
	if !manual {
		t.Fatalf("manual mode should be on when access_token is present")
	}
	if cred.AccessToken != "token-abc" || cred.AccountID != "acct-1" || cred.Email != "a@example.com" {
		t.Fatalf("credential = %#v", cred)
	}
	if acc.Source != "manual" {
		t.Fatalf("account source = %q, want manual", acc.Source)
	}
}

func TestParseQueryRequestRejectsMalformedNonEmptyJSON(t *testing.T) {
	if _, err := parseQueryRequest([]byte(`{"auth_index":`)); err == nil {
		t.Fatal("parseQueryRequest() error = nil, want malformed JSON error")
	}
	response := handleRedeem(pluginapi.ManagementRequest{Body: []byte(`{"auth_index":`)})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("handleRedeem malformed status = %d, want 400", response.StatusCode)
	}
}

func TestLegacyInviteFallbackOnlyForDefinitiveUnsupportedStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone, http.StatusNotImplemented} {
		if !legacyInviteFallbackStatus(status) {
			t.Fatalf("legacyInviteFallbackStatus(%d) = false, want true", status)
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		if legacyInviteFallbackStatus(status) {
			t.Fatalf("legacyInviteFallbackStatus(%d) = true, want false", status)
		}
	}
}

func TestSendInviteDoesNotRetryLegacyAfterAmbiguousV2Failure(t *testing.T) {
	requests := 0
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"uncertain"}`))
	}))
	defer proxyServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := sendInvite(ctx, pluginConfig{BaseURL: "http://chatgpt.example"}, codexCredential{AccessToken: "token"}, accountInfo{}, []string{"x@example.com"}, "legacy", "", proxyServer.URL)
	if err == nil {
		t.Fatal("sendInvite() error = nil, want V2 failure")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one V2 request and no legacy retry", requests)
	}
}

func TestWorkspaceReferralProgramIsSelected(t *testing.T) {
	if got := inferReferralProgram(map[string]any{"account_type": "workspace"}); got != programIDWorkspace {
		t.Fatalf("inferReferralProgram(workspace) = %q", got)
	}
	if got := referralProgramForAccount(accountInfo{ReferralProgramID: programIDWorkspace}); got != programIDWorkspace {
		t.Fatalf("referralProgramForAccount() = %q", got)
	}

	seenBody := make(chan string, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seenBody <- string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"invites":[]}`))
	}))
	defer proxyServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := sendInviteV2(ctx, pluginConfig{BaseURL: "http://chatgpt.example"}, codexCredential{AccessToken: "token"}, accountInfo{ReferralProgramID: programIDWorkspace}, []string{"x@example.com"}, "", proxyServer.URL)
	if err != nil || !result.OK {
		t.Fatalf("sendInviteV2() result=%#v err=%v", result, err)
	}
	if body := <-seenBody; !strings.Contains(body, `"program_id":"codex_referral_workspace"`) {
		t.Fatalf("workspace invite body = %s", body)
	}
}

func TestLiftEligibilitySeparatesRewardCapacityFromMaxInvites(t *testing.T) {
	var result referralsResponse
	liftEligibilityFields(&result, []byte(`{"remaining_send_capacity":3,"remaining_reward_capacity":2,"max_send_capacity":5}`))
	if result.RemainingInvites != float64(3) || result.RemainingRewardCapacity != float64(2) || result.MaxInvites != float64(5) {
		t.Fatalf("eligibility projection = %#v", result)
	}
}

func TestReferralCapacityProbesDedicatedCreditsBeforeUsageFallback(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == referralsCreditsEndpointPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"remaining_invites":4,"max_invites":8}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer proxyServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := fetchReferralCapacity(ctx, pluginConfig{BaseURL: "http://chatgpt.example"}, codexCredential{AccessToken: "token"}, accountInfo{}, "", proxyServer.URL)
	if err != nil {
		t.Fatalf("fetchReferralCapacity() error = %v", err)
	}
	if !result.ReferralCreditsHit || result.UsageEndpointUsed {
		t.Fatalf("referral result = %#v", result)
	}
	if result.RemainingInvites != float64(4) || result.MaxInvites != float64(8) {
		t.Fatalf("credits projection = %#v", result)
	}
}

func TestReferralCapacityFailsClosedWhenEveryProbeFails(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer proxyServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := fetchReferralCapacity(ctx, pluginConfig{BaseURL: "http://chatgpt.example"}, codexCredential{AccessToken: "token"}, accountInfo{}, "", proxyServer.URL)
	if err == nil {
		t.Fatal("fetchReferralCapacity() error = nil, want all-probes-failed error")
	}
}

func TestBuildProxyDialerAcceptsSocks5HAlias(t *testing.T) {
	if _, _, err := buildProxyDialer("socks5h://127.0.0.1:1080"); err != nil {
		t.Fatalf("buildProxyDialer(socks5h) error = %v", err)
	}
}

type cancelAwareDialer struct{}

func (cancelAwareDialer) Dial(string, string) (net.Conn, error) {
	return nil, errors.New("context-free Dial must not be used")
}

func (cancelAwareDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestUtlsConnectionSetupHonorsContextCancellation(t *testing.T) {
	rt := newUtlsRoundTripper(cancelAwareDialer{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := rt.createConnection(ctx, "chatgpt.com", "chatgpt.com:443")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("createConnection() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("createConnection() cancellation took %v", elapsed)
	}
}

func TestHTTPSProxyTLSForcesHTTP11ALPN(t *testing.T) {
	cfg := proxyTLSConfig("proxy.example")
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
		t.Fatalf("proxy TLS ALPN = %#v, want only http/1.1", cfg.NextProtos)
	}
}

func TestRenderUsagePageIncludesNonPersistentCookieInput(t *testing.T) {
	page := renderUsagePage(defaultConfig())
	for _, want := range []string{`id="cookie"`, `cookie: cookieInput.value.trim()`} {
		if !strings.Contains(page, want) {
			t.Fatalf("usage page missing %q", want)
		}
	}
	if strings.Contains(page, "codex-usage-cookie") {
		t.Fatal("usage page persists Cookie in localStorage")
	}
}
