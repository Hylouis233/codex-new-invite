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
		"function badgeValue(text, cls)",
		"span.textContent = v.badgeText",
		"dd.textContent = v == null ? '' : String(v)",
		"setAccountPlaceholder(t('account.loadFailed'",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("usage page is missing safe DOM marker %q", required)
		}
	}
	for _, forbidden := range []string{
		"dd.innerHTML = v",
		"'<strong>' + String(el.title)",
		"String(el.description) + '</span>'",
		"el.rules.map(r => '• ' + r)",
		"'<span style=\"white-space:pre-wrap\">' + d.note",
	} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("usage page still contains unsafe dynamic HTML marker %q", forbidden)
		}
	}
}
