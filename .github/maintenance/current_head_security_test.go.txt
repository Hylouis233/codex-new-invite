package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type securityRoundTripFunc func(*http.Request) (*http.Response, error)

func (f securityRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCurrentHeadDispatchPinsCredentialOrigin(t *testing.T) {
	for _, origin := range []string{"https://other.example", "http://chatgpt.com", "https://chatgpt.com@other.example", "https://chatgpt.com/path"} {
		raw, _ := json.Marshal(map[string]any{"base_url": origin, "cdks": []string{"fixture-cdk"}})
		var req dispatchRequest
		if err := json.Unmarshal(raw, &req); err == nil {
			t.Errorf("accepted noncanonical credential origin %q", origin)
		}
		resp := handleDispatch(pluginapi.ManagementRequest{Body: raw})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("dispatch status=%d, want 400", resp.StatusCode)
		}
	}
	var valid dispatchRequest
	if err := json.Unmarshal([]byte(`{"base_url":"https://chatgpt.com/","auth_name":"fixture.json"}`), &valid); err != nil || valid.BaseURL != defaultBaseURL {
		t.Fatalf("canonical dispatch rejected: %+v %v", valid, err)
	}
}

func TestCurrentHeadQueryRoutesRejectInvalidBodiesBeforeCredentials(t *testing.T) {
	old := localManagementClient
	defer func() { localManagementClient = old }()
	var requests atomic.Int32
	localManagementClient = &http.Client{Transport: securityRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, fmt.Errorf("unexpected credential lookup")
	})}
	handlers := map[string]func(pluginapi.ManagementRequest) pluginapi.ManagementResponse{
		"usage": handleUsage, "referrals": handleReferrals, "probe": handleProbe, "redeem": handleRedeem,
	}
	bodies := []struct {
		name, body string
		status     int
	}{
		{"truncated", `{"auth_index":`, 400},
		{"null", `null`, 400},
		{"array", `[]`, 400},
		{"wrong-field-type", `{"auth_index":7}`, 400},
		{"invalid-origin", `{"base_url":"https://other.example"}`, 400},
		{"oversized-whitespace", strings.Repeat(" ", maxManagementBodyBytes+1), 413},
	}
	for name, handler := range handlers {
		for _, tc := range bodies {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				r := pluginapi.ManagementRequest{Body: []byte(tc.body), Headers: http.Header{
					"Authorization": []string{"Bearer fixture-management"}, "Origin": []string{"http://127.0.0.1:8317"},
				}}
				resp := handler(r)
				if resp.StatusCode != tc.status {
					t.Errorf("status=%d, want %d", resp.StatusCode, tc.status)
				}
			})
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid bodies reached credential lookup %d times", requests.Load())
	}
}

func TestCurrentHeadCDKDispatchRequiresExplicitDestination(t *testing.T) {
	old := currentConfig()
	defer activeConfig.Store(old)
	activeConfig.Store(defaultConfig())
	if currentConfig().CDKSiteURL != "" {
		t.Fatal("a CDK recipient is selected by default")
	}
	resp := handleDispatch(pluginapi.ManagementRequest{Body: []byte(`{"cdks":["fixture-cdk"],"auth_name":"fixture.json"}`)})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured dispatch status=%d, want 503", resp.StatusCode)
	}
	for _, raw := range []string{"http://cards.example", "https://user:pass@cards.example", "https://cards.example/path", "https://cards.example?", "https://cards.example#fragment", "https:///missing"} {
		if got := normalizeConfig(pluginConfig{CDKSiteURL: raw}).CDKSiteURL; got != "" {
			t.Errorf("unsafe CDK origin enabled: %q", got)
		}
	}
	if got := normalizeConfig(pluginConfig{CDKSiteURL: "https://cards.example/"}).CDKSiteURL; got != "https://cards.example" {
		t.Errorf("valid origin=%q", got)
	}
	cfg := defaultConfig()
	cfg.CDKSiteURL = "https://cards.example"
	activeConfig.Store(cfg)
	resp = handleDispatch(pluginapi.ManagementRequest{Body: []byte(`{"cdks":["fixture-cdk"]}`)})
	if resp.StatusCode != 400 || !strings.Contains(string(resp.Body), "explicit managed account") {
		t.Fatalf("dispatch did not reject unspecified identity before lookup: %d", resp.StatusCode)
	}
}

func TestCurrentHeadCDKRedirectAndResponseBoundaries(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			hits := 0
			client := &http.Client{Transport: securityRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				hits++
				if r.URL.Host != "cards.example" {
					t.Fatal("redirect target was contacted")
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Location": []string{"https://other.example/receive"}}, Body: io.NopCloser(strings.NewReader(`{"status":"redirect"}`)), Request: r}, nil
			})}
			_, status, err := cdkSiteCall(context.Background(), client, "https://cards.example", "/api/lookup", map[string]string{"cdk": "fixture"})
			if err != nil || status != code || hits != 1 {
				t.Fatalf("redirect was not preserved: status=%d hits=%d err=%v", status, hits, err)
			}
			if client.CheckRedirect != nil {
				t.Fatal("caller client was mutated")
			}
		})
	}
	for _, content := range []string{"null", "[]", "not-json", "", strings.Repeat("x", int(upstreamBodyLimit)+1)} {
		client := &http.Client{Transport: securityRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(content)), Header: make(http.Header), Request: r}, nil
		})}
		if _, _, err := cdkSiteCall(context.Background(), client, "https://cards.example", "/api/lookup", map[string]string{"cdk": "fixture"}); err == nil {
			t.Fatalf("malformed/oversized response accepted (%d bytes)", len(content))
		}
	}
}

func TestCurrentHeadActivationHooksAreRetired(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var reg managementRegistrationResponse
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	for _, route := range reg.Routes {
		if strings.Contains(route.Path, "activate") {
			t.Fatalf("legacy route is advertised: %s", route.Path)
		}
	}
	for name, handler := range map[string]func(pluginapi.ManagementRequest) pluginapi.ManagementResponse{
		"activate": handleActivate, "cdp": handleCDPActivate, "desktop": handleDesktopActivate,
	} {
		resp := handler(pluginapi.ManagementRequest{Body: []byte(`{"email":"fixture@example.com","card":"fixture-only"}`)})
		if resp.StatusCode != http.StatusGone {
			t.Errorf("%s status=%d, want 410", name, resp.StatusCode)
		}
	}
	page := renderUsagePage(defaultConfig())
	for _, marker := range []string{`id="activate"`, `id="desktopActivate"`, `id="activateCard"`, `activateAccount`, `/_full_v5.py`} {
		if strings.Contains(page, marker) {
			t.Errorf("unsupported UI action remains: %s", marker)
		}
	}
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"exec.Command", "MAIL_" + "AUTH", "_full_" + "v5.py", "_cdp_" + "auto.py"} {
		if strings.Contains(string(source), marker) {
			t.Errorf("machine-local hook marker remains: %s", marker)
		}
	}
}

func TestCurrentHeadAmbiguousInviteDoesNotFallBack(t *testing.T) {
	for _, status := range []int{400, 401, 403, 409, 429, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"fixture rejection"}`))
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := sendInvite(ctx, pluginConfig{BaseURL: "http://chatgpt.example"}, codexCredential{AccessToken: "fixture-token"}, accountInfo{}, []string{"fixture@example.com"}, "fixture", "", server.URL)
			if err == nil || hits.Load() != 1 {
				t.Fatalf("status=%d was retried or hidden: hits=%d err=%v", status, hits.Load(), err)
			}
		})
	}
}
