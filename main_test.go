package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		http.MethodPost + " /codex-invite/auto-assign",
		http.MethodPost + " /codex-invite/dispatch",
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

func TestPlanAutoAssignmentFillsHighCapacityFirst(t *testing.T) {
	entries := []autoAssignAccount{
		{Account: accountInfo{Name: "low", Email: "low@example.com"}, Capacity: 2},
		{Account: accountInfo{Name: "high", Email: "high@example.com"}, Capacity: 5},
		{Account: accountInfo{Name: "mid", Email: "mid@example.com"}, Capacity: 3},
	}
	sortAutoAssignAccounts(entries)
	unassigned := planAutoAssignment(entries, []string{
		"e1@example.com", "e2@example.com", "e3@example.com", "e4@example.com",
		"e5@example.com", "e6@example.com", "e7@example.com", "e8@example.com",
		"e9@example.com", "e10@example.com",
	}, 0)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %#v, want empty", unassigned)
	}
	if got := len(entries[0].AssignedEmails); got != 5 {
		t.Fatalf("high-capacity account got %d emails, want 5", got)
	}
	if got := len(entries[1].AssignedEmails); got != 3 {
		t.Fatalf("mid-capacity account got %d emails, want 3", got)
	}
	if got := len(entries[2].AssignedEmails); got != 2 {
		t.Fatalf("low-capacity account got %d emails, want 2", got)
	}
}

func TestPlanAutoAssignmentRespectsPerAccountLimitAndUnassigned(t *testing.T) {
	entries := []autoAssignAccount{
		{Account: accountInfo{Name: "a", Email: "a@example.com"}, Capacity: 5},
		{Account: accountInfo{Name: "b", Email: "b@example.com"}, Capacity: 5},
	}
	sortAutoAssignAccounts(entries)
	emails := []string{"e1@example.com", "e2@example.com", "e3@example.com"}
	unassigned := planAutoAssignment(entries, emails, 1)
	if len(unassigned) != 1 || unassigned[0] != "e3@example.com" {
		t.Fatalf("unassigned = %#v, want [e3@example.com]", unassigned)
	}
	if len(entries[0].AssignedEmails) != 1 || len(entries[1].AssignedEmails) != 1 {
		t.Fatalf("per-account limit not respected: %#v / %#v", entries[0].AssignedEmails, entries[1].AssignedEmails)
	}
}

func TestPlanAutoAssignmentSkipsExhaustedAccounts(t *testing.T) {
	entries := []autoAssignAccount{
		{Account: accountInfo{Name: "empty", Email: "empty@example.com"}, Capacity: 0, Status: "skipped"},
		{Account: accountInfo{Name: "ok", Email: "ok@example.com"}, Capacity: 1},
	}
	unassigned := planAutoAssignment(entries, []string{"e1@example.com", "e2@example.com"}, 0)
	if len(entries[0].AssignedEmails) != 0 {
		t.Fatalf("exhausted account should not receive emails")
	}
	if len(entries[1].AssignedEmails) != 1 {
		t.Fatalf("ok account should receive one email")
	}
	if len(unassigned) != 1 || unassigned[0] != "e2@example.com" {
		t.Fatalf("unassigned = %#v, want [e2@example.com]", unassigned)
	}
}

func TestSortAutoAssignAccountsOrdersByCapacityThenCredits(t *testing.T) {
	high := 100.0
	low := 1.0
	entries := []autoAssignAccount{
		{Account: accountInfo{Name: "n", Email: "n@example.com"}, Capacity: 3, CreditBalance: &low},
		{Account: accountInfo{Name: "b", Email: "b@example.com"}, Capacity: 3, CreditBalance: &high},
		{Account: accountInfo{Name: "z", Email: "z@example.com"}, Capacity: 7},
	}
	sortAutoAssignAccounts(entries)
	if entries[0].Account.Name != "z" || entries[1].Account.Name != "b" || entries[2].Account.Name != "n" {
		t.Fatalf("order = %s,%s,%s; want z,b,n", entries[0].Account.Name, entries[1].Account.Name, entries[2].Account.Name)
	}
}

func TestCapacityFromEligibility(t *testing.T) {
	capacity, ok := capacityFromEligibility([]byte(`{"remaining_send_capacity": 4, "remaining_reward_capacity": 2}`))
	if !ok || capacity != 4 {
		t.Fatalf("capacity = %d ok=%v, want 4 true", capacity, ok)
	}
	if _, ok := capacityFromEligibility([]byte(`{"should_show": true}`)); ok {
		t.Fatalf("missing remaining_send_capacity should not report ok")
	}
	if _, ok := capacityFromEligibility([]byte(`not json`)); ok {
		t.Fatalf("invalid JSON should not report ok")
	}
}

func TestInviteCountFromTracking(t *testing.T) {
	count, ok := inviteCountFromTracking([]byte(`{"items":[{"email":"a@x.com"},{"email":"b@x.com"},{"referral_id":"r"}]}`))
	if !ok || count != 2 {
		t.Fatalf("count = %d ok=%v, want 2 true", count, ok)
	}
}

func TestFilterAccounts(t *testing.T) {
	accounts := []accountInfo{
		{AuthIndex: "0", Name: "codex-a.json", Email: "alice@example.com"},
		{AuthIndex: "1", Name: "codex-b.json", Email: "bob@example.com"},
	}
	if got := filterAccounts(accounts, nil); len(got) != 2 {
		t.Fatalf("no filter should keep all accounts")
	}
	if got := filterAccounts(accounts, []string{"bob@example.com"}); len(got) != 1 || got[0].Name != "codex-b.json" {
		t.Fatalf("email substring filter failed: %#v", got)
	}
	if got := filterAccounts(accounts, []string{"codex-a"}); len(got) != 1 || got[0].AuthIndex != "0" {
		t.Fatalf("name substring filter failed: %#v", got)
	}
	if got := filterAccounts(accounts, []string{"nobody"}); len(got) != 0 {
		t.Fatalf("non-matching filter should drop all accounts")
	}
}

func TestChunkEmails(t *testing.T) {
	chunks := chunkEmails([]string{"1@x.com", "2@x.com", "3@x.com"}, 2)
	if len(chunks) != 2 || len(chunks[0]) != 2 || len(chunks[1]) != 1 {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestCollectRawEmailsDedupesAndValidates(t *testing.T) {
	emails, err := collectRawEmails([]string{"A@x.com", "a@x.com"}, "b@x.com", 10)
	if err != nil || len(emails) != 2 {
		t.Fatalf("emails = %#v err = %v, want 2 unique", emails, err)
	}
	if _, err := collectRawEmails(nil, "bad@@x.com", 10); err == nil {
		t.Fatalf("invalid email should fail")
	}
}

func TestCollectCDKsNormalizesAndDedupes(t *testing.T) {
	cdks := collectCDKs([]string{"ca-ku9k-ghyh-g1lx-uycu", "CA-TJ5Y-EG5G-MPH0-YVJT"}, "CA-KU9K-GHYH-G1LX-UYCU\n  ca-1656-bgbf-jvkf-0hi9,,")
	want := []string{"CA-KU9K-GHYH-G1LX-UYCU", "CA-TJ5Y-EG5G-MPH0-YVJT", "CA-1656-BGBF-JVKF-0HI9"}
	if len(cdks) != len(want) {
		t.Fatalf("cdks = %#v, want %#v", cdks, want)
	}
	for i := range want {
		if cdks[i] != want[i] {
			t.Fatalf("cdks[%d] = %q, want %q", i, cdks[i], want[i])
		}
	}
}

func TestInviteCountFromTrackingMonthlyWindow(t *testing.T) {
	old := time.Now().AddDate(0, 0, -45).Format(time.RFC3339)
	recent := time.Now().AddDate(0, 0, -3).Format(time.RFC3339)
	body := fmt.Sprintf(`{"items":[{"email":"old@x.com","created_at":%q},{"email":"new@x.com","created_at":%q},{"email":"nodate@x.com"},{"referral_id":"r"}]}`, old, recent)
	count, ok := inviteCountFromTracking([]byte(body))
	if !ok || count != 2 {
		t.Fatalf("count = %d ok=%v, want 2 true (recent + no-date; 45-day-old excluded)", count, ok)
	}
}

func TestDispatchCDKSiteURLOnlyFromConfig(t *testing.T) {
	// The dispatch flow may only talk to the configured site; a non-https override is
	// normalized back to the default so CDKs never leak to a cleartext origin.
	cfg := normalizeConfig(pluginConfig{CDKSiteURL: "http://evil.example"})
	if cfg.CDKSiteURL != defaultCDKSiteURL {
		t.Fatalf("cdk site = %q, want default %q", cfg.CDKSiteURL, defaultCDKSiteURL)
	}
	cfg = normalizeConfig(pluginConfig{CDKSiteURL: "https://cards.example.com/"})
	if cfg.CDKSiteURL != "https://cards.example.com" {
		t.Fatalf("cdk site = %q, want trimmed https origin", cfg.CDKSiteURL)
	}
}
