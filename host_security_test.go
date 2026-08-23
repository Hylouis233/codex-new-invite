package main

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
		"http://127.0.0.1:8317":                   "http://127.0.0.1:8317",
		"http://127.42.0.9:8317":                  "http://127.42.0.9:8317",
		"https://[::1]:8317/management":           "https://[::1]:8317",
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
