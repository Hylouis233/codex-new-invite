package main

import (
	"strings"
	"testing"
)

func TestUsagePageDoesNotPersistManagementKey(t *testing.T) {
	page := renderUsagePage(defaultConfig())
	for _, forbidden := range []string{
		"codex-usage-mgmt-key-v1",
		"storedManagementKey",
		"persistManagementKey",
		"localStorage.setItem(MGMT_KEY_STORE",
	} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("usage page still contains management-key persistence marker %q", forbidden)
		}
	}
}

func TestUsagePageTreatsDynamicMetricsAsText(t *testing.T) {
	page := renderUsagePage(defaultConfig())
	for _, required := range []string{
		"function metricValue(v)",
		"dd.textContent = v == null ? '' : String(v)",
		"span.textContent = part.badge.text",
		"failOpt.textContent = t('account.loadFailed'",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("usage page is missing safe DOM marker %q", required)
		}
	}
	for _, forbidden := range []string{
		"dd.innerHTML = v",
		"'<span class=\"badge ' + cls + '\">'",
		"'<strong>' + String(el.title) + '</strong>'",
		"'<span style=\"white-space:pre-wrap\">'",
	} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("usage page still contains unsafe dynamic HTML marker %q", forbidden)
		}
	}
	if strings.Contains(page, "const DEFAULTS = `{") {
		t.Fatal("usage defaults are still emitted as a template-literal string")
	}
}

func TestInvitePageRestrictsInviteLinksToHTTPS(t *testing.T) {
	page := renderInvitePage(defaultConfig())
	for _, required := range []string{
		"function safeInviteURL(raw)",
		"if (candidate.protocol !== 'https:') return '';",
		"if (candidate.username || candidate.password) return '';",
		"const inviteURL = safeInviteURL(invite.invite_url);",
		"link.href = inviteURL;",
		"link.rel = 'noopener noreferrer';",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("invite page is missing safe URL marker %q", required)
		}
	}
	for _, forbidden := range []string{
		"link.href = invite.invite_url;",
		"link.rel = 'noreferrer';",
	} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("invite page still contains unsafe URL marker %q", forbidden)
		}
	}
}

func TestRegistrationMetadataMatchesFork(t *testing.T) {
	reg := pluginRegistration()
	if reg.Metadata.Author != "Hylouis233" || reg.Metadata.GitHubRepository != "https://github.com/Hylouis233/codex-new-invite" {
		t.Fatalf("metadata = %#v", reg.Metadata)
	}
}
