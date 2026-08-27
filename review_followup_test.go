package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestReviewFollowupRegistrationMatchesPublishedFork(t *testing.T) {
	reg := pluginRegistration()
	if reg.Metadata.Author != "Hylouis233" || reg.Metadata.GitHubRepository != "https://github.com/Hylouis233/codex-new-invite" {
		t.Fatalf("metadata = %#v", reg.Metadata)
	}
}

func TestReviewFollowupCredentialClientRejectsRedirects(t *testing.T) {
	client, err := inviteHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	if client.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://chatgpt.com/backend-api/codex/usage", nil)
	if err := client.CheckRedirect(req, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirect error = %v, want http.ErrUseLastResponse", err)
	}
}

func TestReviewFollowupRedeemRequiresExplicitIdentity(t *testing.T) {
	response := handleRedeem(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	manual := handleRedeem(pluginapi.ManagementRequest{Body: []byte(`{"access_token":"manual-token"}`)})
	if manual.StatusCode == http.StatusBadRequest && strings.Contains(string(manual.Body), "explicit managed account") {
		t.Fatalf("manual credential was rejected by explicit-account guard: %s", manual.Body)
	}
}

func TestReviewFollowupCapacityProvenance(t *testing.T) {
	var result referralsResponse
	fields := liftEligibilityFields(&result, []byte(`{"remaining_send_capacity":4,"max_send_capacity":10}`))
	if !fields.remaining || !fields.maximum || result.RemainingInvitesSource != "invite/eligibility" || result.MaxInvitesSource != "invite/eligibility" {
		t.Fatalf("eligibility provenance = %#v", result)
	}
	result = referralsResponse{}
	fields = referralCapacityFields{}
	liftReferralFieldsIfMissing(&result, []byte(`{"available_count":3,"limit":9}`), "credits", &fields)
	if result.RemainingInvitesSource != "credits" || result.MaxInvitesSource != "credits" {
		t.Fatalf("fallback provenance = %#v", result)
	}
}

func TestReviewFollowupUsagePageContracts(t *testing.T) {
	cfg := defaultConfig()
	cfg.Language = "en-US"
	cfg.Originator = "Configured Origin"
	cfg.UserAgent = "Configured Agent"
	page := renderUsagePage(cfg)
	wants := []string{
		`const DEFAULTS = {`,
		`"originator":"Configured Origin"`,
		`id="credentialSource"`,
		`id="manualToken"`,
		`id="manualAccountId"`,
		`id="proxyUrl"`,
		`access_token: token`,
		`proxy_url: proxyInput.value.trim()`,
		`accountSelect.addEventListener('change', clearRedeemState)`,
		`remaining_invites_source`,
		`max_invites_source`,
	}
	for _, want := range wants {
		if !strings.Contains(page, want) {
			t.Fatalf("usage page missing %q", want)
		}
	}
	if strings.Contains(page, "const DEFAULTS = `{") {
		t.Fatal("usage defaults are still emitted as a template-literal string")
	}
}
