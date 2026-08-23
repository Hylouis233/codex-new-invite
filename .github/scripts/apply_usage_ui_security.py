from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected one match, found {count}")
    return text.replace(old, new, 1)


main_path = Path("main.go")
text = main_path.read_text(encoding="utf-8")

text = replace_once(
    text,
    "    .metric dd { margin: 0; font-size: 14px; font-weight: 600; word-break: break-word; }",
    "    .metric dd { margin: 0; font-size: 14px; font-weight: 600; word-break: break-word; white-space: pre-wrap; }",
    "metric whitespace",
)
text = replace_once(
    text,
    "    const MGMT_KEY_STORE = 'codex-usage-mgmt-key-v1';\n",
    "",
    "management key storage constant",
)
text = replace_once(
    text,
    """    const keyInput = document.getElementById('managementKey');
    function storedManagementKey() {
      try { return localStorage.getItem(MGMT_KEY_STORE) || ''; } catch { return ''; }
    }
    function persistManagementKey(value) {
      try {
        if (value) { localStorage.setItem(MGMT_KEY_STORE, value); } else { localStorage.removeItem(MGMT_KEY_STORE); }
      } catch (error) { /* ignore storage failures */ }
    }
""",
    """    const keyInput = document.getElementById('managementKey');
""",
    "management key persistence helpers",
)
text = replace_once(
    text,
    """    function pctBadge(pct) {
      const n = Number(pct);
      if (!Number.isFinite(n)) return fmtNumber(pct);
      const cls = n >= 90 ? 'err' : n >= 70 ? 'warn' : 'ok';
      return '<span class=\"badge ' + cls + '\">' + n.toFixed(1) + '%</span>';
    }
    function setMetric(rows) {
      metrics.innerHTML = '';
      for (const [k, v] of rows) {
        const dt = document.createElement('dt'); dt.textContent = k;
        const dd = document.createElement('dd'); dd.innerHTML = v;
        metrics.appendChild(dt); metrics.appendChild(dd);
      }
    }
""",
    """    function badgeValue(text, cls) {
      const allowedClass = cls === 'ok' || cls === 'warn' || cls === 'err' ? cls : '';
      return { badgeText: String(text), badgeClass: allowedClass };
    }
    function pctBadge(pct) {
      const n = Number(pct);
      if (!Number.isFinite(n)) return fmtNumber(pct);
      const cls = n >= 90 ? 'err' : n >= 70 ? 'warn' : 'ok';
      return badgeValue(n.toFixed(1) + '%', cls);
    }
    function setMetric(rows) {
      metrics.innerHTML = '';
      for (const [k, v] of rows) {
        const dt = document.createElement('dt'); dt.textContent = k;
        const dd = document.createElement('dd');
        if (v && typeof v === 'object' && Object.prototype.hasOwnProperty.call(v, 'badgeText')) {
          const span = document.createElement('span');
          span.className = 'badge' + (v.badgeClass ? ' ' + v.badgeClass : '');
          span.textContent = v.badgeText;
          dd.appendChild(span);
        } else {
          dd.textContent = v == null ? '' : String(v);
        }
        metrics.appendChild(dt); metrics.appendChild(dd);
      }
    }
""",
    "metric DOM renderer",
)
text = replace_once(
    text,
    """    const rawPre = document.getElementById('raw');

    async function readJSON(response) {
""",
    """    const rawPre = document.getElementById('raw');

    function setAccountPlaceholder(message) {
      accountSelect.innerHTML = '';
      const option = document.createElement('option');
      option.textContent = String(message || '');
      accountSelect.appendChild(option);
    }

    async function readJSON(response) {
""",
    "account placeholder helper",
)
text = replace_once(
    text,
    "        if (!accountSelect.options.length) accountSelect.innerHTML = '<option>' + t('account.placeholderEmpty') + '</option>';",
    "        if (!accountSelect.options.length) setAccountPlaceholder(t('account.placeholderEmpty'));",
    "empty account placeholder",
)
text = replace_once(
    text,
    "        accountSelect.innerHTML = '<option>' + t('account.loadFailed', { error: (e.message || e) }) + '</option>';",
    "        setAccountPlaceholder(t('account.loadFailed', { error: String(e.message || e) }));",
    "account load error placeholder",
)
text = replace_once(
    text,
    "      rows.push([t('metric.httpStatus'), '<span class=\"badge ' + (d.ok ? 'ok' : 'err') + '\">' + d.status_code + '</span>']);",
    "      rows.push([t('metric.httpStatus'), badgeValue(fmtNumber(d.status_code), d.ok ? 'ok' : 'err')]);",
    "HTTP status badge",
)
text = replace_once(
    text,
    "        if (el.title) rows.push([t('metric.offerTitle'), '<strong>' + String(el.title) + '</strong>']);",
    "        if (el.title) rows.push([t('metric.offerTitle'), String(el.title)]);",
    "offer title",
)
text = replace_once(
    text,
    "        if (el.description) rows.push([t('metric.offerDesc'), '<span style=\"white-space:pre-wrap\">' + String(el.description) + '</span>']);",
    "        if (el.description) rows.push([t('metric.offerDesc'), String(el.description)]);",
    "offer description",
)
text = replace_once(
    text,
    "          rows.push([t('metric.rules'), '<span style=\"white-space:pre-wrap\">' + el.rules.map(r => '• ' + r).join('\\n') + '</span>']);",
    "          rows.push([t('metric.rules'), el.rules.map(r => '• ' + String(r)).join('\\n')]);",
    "offer rules",
)
text = replace_once(
    text,
    "        rows.push([t('metric.rateLimitStatus'), '<span class=\"badge ' + (limitReached ? 'err' : 'ok') + '\">' + (limitReached ? t('metric.rateLimitExhausted') : t('metric.rateLimitActive')) + '</span>']);",
    "        rows.push([t('metric.rateLimitStatus'), badgeValue(limitReached ? t('metric.rateLimitExhausted') : t('metric.rateLimitActive'), limitReached ? 'err' : 'ok')]);",
    "rate-limit badge",
)
text = replace_once(
    text,
    "      if (d.note) rows.push([t('metric.note'), '<span style=\"white-space:pre-wrap\">' + d.note + '</span>']);",
    "      if (d.note) rows.push([t('metric.note'), String(d.note)]);",
    "referral note",
)
text = replace_once(
    text,
    "        accountSelect.innerHTML = '<option>' + t('account.placeholderKey') + '</option>';",
    "        setAccountPlaceholder(t('account.placeholderKey'));",
    "missing key placeholder",
)
text = replace_once(
    text,
    """    keyInput.addEventListener('input', () => persistManagementKey(keyInput.value.trim()));

    applyLocale();
    // Restore a previously saved key and auto-load accounts only when present.
    const savedKey = storedManagementKey();
    if (savedKey) { keyInput.value = savedKey; loadAccounts(); }
    else { accountSelect.innerHTML = '<option>' + t('account.placeholderKey') + '</option>'; }
""",
    """    applyLocale();
    setAccountPlaceholder(t('account.placeholderKey'));
""",
    "management key restore block",
)

forbidden = (
    "MGMT_KEY_STORE",
    "storedManagementKey",
    "persistManagementKey",
    "dd.innerHTML = v",
    "'<strong>' + String(el.title)",
    "String(el.description) + '</span>'",
    "el.rules.map(r => '• ' + r)",
    "'<span style=\"white-space:pre-wrap\">' + d.note",
)
remaining = [marker for marker in forbidden if marker in text]
if remaining:
    raise SystemExit(f"unsafe usage-page markers remain: {remaining}")
main_path.write_text(text, encoding="utf-8", newline="\n")

Path("ui_security_test.go").write_text(
    '''package main

import (
\t"strings"
\t"testing"
)

func TestUsagePageDoesNotPersistManagementKey(t *testing.T) {
\tpage := renderUsagePage(defaultConfig())
\tfor _, forbidden := range []string{
\t\t"codex-usage-mgmt-key-v1",
\t\t"storedManagementKey",
\t\t"persistManagementKey",
\t\t"localStorage.setItem(MGMT_KEY_STORE",
\t} {
\t\tif strings.Contains(page, forbidden) {
\t\t\tt.Fatalf("usage page still contains management-key persistence marker %q", forbidden)
\t\t}
\t}
}

func TestUsagePageTreatsDynamicMetricsAsText(t *testing.T) {
\tpage := renderUsagePage(defaultConfig())
\tfor _, required := range []string{
\t\t"function badgeValue(text, cls)",
\t\t"span.textContent = v.badgeText",
\t\t"dd.textContent = v == null ? '' : String(v)",
\t\t"setAccountPlaceholder(t('account.loadFailed'",
\t} {
\t\tif !strings.Contains(page, required) {
\t\t\tt.Fatalf("usage page is missing safe DOM marker %q", required)
\t\t}
\t}
\tfor _, forbidden := range []string{
\t\t"dd.innerHTML = v",
\t\t"'<strong>' + String(el.title)",
\t\t"String(el.description) + '</span>'",
\t\t"el.rules.map(r => '• ' + r)",
\t\t"'<span style=\\\"white-space:pre-wrap\\\">' + d.note",
\t} {
\t\tif strings.Contains(page, forbidden) {
\t\t\tt.Fatalf("usage page still contains unsafe dynamic HTML marker %q", forbidden)
\t\t}
\t}
}
''',
    encoding="utf-8",
    newline="\n",
)

readme_path = Path("README.md")
readme = readme_path.read_text(encoding="utf-8")
readme = replace_once(
    readme,
    "| **Usage page fixes** | Added management-key input, `X-Codex-Invite-Origin` header, GET→POST for fetch-body support, and account-status metrics on the referrals fallback path. |",
    "| **Usage page fixes** | Added a non-persistent management-key input, `X-Codex-Invite-Origin` header, safe text-node rendering for dynamic metrics, GET→POST for fetch-body support, and account-status metrics on the referrals fallback path. |",
    "README usage-page security",
)
readme = replace_once(
    readme,
    "3. Enter your CPA management key (saved to localStorage for convenience).",
    "3. Enter your CPA management key. It remains only in the current page session and is not persisted to localStorage.",
    "README management key persistence",
)
readme_path.write_text(readme, encoding="utf-8", newline="\n")
