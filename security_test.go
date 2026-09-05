package main

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNormalizeCredentialBaseURL(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty uses configured default", raw: "", want: ""},
		{name: "canonical", raw: "https://chatgpt.com", want: defaultBaseURL},
		{name: "canonical trailing slash", raw: " https://chatgpt.com/ ", want: defaultBaseURL},
	}
	for _, test := range valid {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeCredentialBaseURL(test.raw)
			if err != nil {
				t.Fatalf("normalizeCredentialBaseURL(%q) returned error: %v", test.raw, err)
			}
			if got != test.want {
				t.Fatalf("normalizeCredentialBaseURL(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}

	invalid := []string{
		"http://chatgpt.com",
		"https://evil.example",
		"https://chatgpt.com.evil.example",
		"https://user@chatgpt.com",
		"https://chatgpt.com:443",
		"https://chatgpt.com/backend-api",
		"https://chatgpt.com?redirect=https://evil.example",
		"https://chatgpt.com#fragment",
	}
	for _, raw := range invalid {
		raw := raw
		t.Run("reject "+raw, func(t *testing.T) {
			t.Parallel()
			if _, err := normalizeCredentialBaseURL(raw); err == nil {
				t.Fatalf("normalizeCredentialBaseURL(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestInviteRequestRejectsCredentialExfiltrationBaseURL(t *testing.T) {
	t.Parallel()

	var request inviteRequest
	err := json.Unmarshal([]byte("{\"access_token\":\"secret\",\"base_url\":\"https://evil.example\"}"), &request)
	if err == nil {
		t.Fatal("invite request with a non-ChatGPT base_url unexpectedly succeeded")
	}
	if request.AccessToken != "" {
		t.Fatal("invite request retained access_token after base_url validation failed")
	}
}

func TestQueryRequestRejectsCredentialExfiltrationBaseURL(t *testing.T) {
	t.Parallel()

	var request queryRequest
	err := json.Unmarshal([]byte("{\"access_token\":\"secret\",\"base_url\":\"https://evil.example\"}"), &request)
	if err == nil {
		t.Fatal("query request with a non-ChatGPT base_url unexpectedly succeeded")
	}
	if request.AccessToken != "" {
		t.Fatal("query request retained access_token after base_url validation failed")
	}
}

func TestCredentialRequestsAcceptCanonicalChatGPTOrigin(t *testing.T) {
	t.Parallel()

	var invite inviteRequest
	if err := json.Unmarshal([]byte("{\"access_token\":\"secret\",\"base_url\":\"https://chatgpt.com/\"}"), &invite); err != nil {
		t.Fatalf("canonical invite base_url rejected: %v", err)
	}
	if invite.BaseURL != defaultBaseURL || invite.AccessToken != "secret" {
		t.Fatalf("unexpected invite request: base_url=%q access_token=%q", invite.BaseURL, invite.AccessToken)
	}

	var query queryRequest
	if err := json.Unmarshal([]byte("{\"access_token\":\"secret\",\"base_url\":\"https://chatgpt.com\"}"), &query); err != nil {
		t.Fatalf("canonical query base_url rejected: %v", err)
	}
	if query.BaseURL != defaultBaseURL || query.AccessToken != "secret" {
		t.Fatalf("unexpected query request: base_url=%q access_token=%q", query.BaseURL, query.AccessToken)
	}
}

func TestPluginConfigRejectsNonChatGPTBaseURL(t *testing.T) {
	t.Parallel()

	var config pluginConfig
	if err := yaml.Unmarshal([]byte("base_url: https://evil.example\nlanguage: en\n"), &config); err == nil {
		t.Fatal("plugin config with a non-ChatGPT base_url unexpectedly succeeded")
	}
}

func TestPluginConfigAcceptsCanonicalChatGPTBaseURL(t *testing.T) {
	t.Parallel()

	var config pluginConfig
	if err := yaml.Unmarshal([]byte("base_url: https://chatgpt.com/\nlanguage: en\n"), &config); err != nil {
		t.Fatalf("canonical plugin base_url rejected: %v", err)
	}
	if config.BaseURL != defaultBaseURL {
		t.Fatalf("config base_url = %q, want %q", config.BaseURL, defaultBaseURL)
	}
	if config.Language != "en" {
		t.Fatalf("config language = %q, want en", config.Language)
	}
}

func TestAutoAssignRequestRejectsCredentialExfiltrationBaseURL(t *testing.T) {
	t.Parallel()

	var request autoAssignRequest
	err := json.Unmarshal([]byte("{\"emails\":[\"a@example.com\"],\"base_url\":\"https://evil.example\"}"), &request)
	if err == nil {
		t.Fatal("auto-assign request with a non-ChatGPT base_url unexpectedly succeeded")
	}
}

func TestAutoAssignRequestAcceptsCanonicalChatGPTOrigin(t *testing.T) {
	t.Parallel()

	var request autoAssignRequest
	if err := json.Unmarshal([]byte("{\"emails\":[\"a@example.com\"],\"base_url\":\"https://chatgpt.com/\"}"), &request); err != nil {
		t.Fatalf("canonical auto-assign base_url rejected: %v", err)
	}
	if request.BaseURL != defaultBaseURL {
		t.Fatalf("auto-assign base_url = %q, want %q", request.BaseURL, defaultBaseURL)
	}
}
