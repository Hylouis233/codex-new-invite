from __future__ import annotations

from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
MAIN = ROOT / "main.go"
STAGED = ROOT / ".github/workflows/apply-current-review-fixes.yml"


def extract_staged_patch() -> str:
    source = STAGED.read_text(encoding="utf-8")
    start_marker = "          python - <<'PY'\n"
    end_marker = "\n          PY\n\n          gofmt -w"
    start = source.find(start_marker)
    if start < 0:
        raise RuntimeError("staged patch start marker not found")
    start += len(start_marker)
    end = source.find(end_marker, start)
    if end < 0:
        raise RuntimeError("staged patch end marker not found")
    lines = source[start:end].splitlines()
    patch = "\n".join(line[10:] if line.startswith("          ") else line for line in lines) + "\n"

    ui_start_marker = "# Usage-page Cookie plumbing and corrected reward-capacity rendering."
    ui_end_marker = "# Keep documentation aligned with the now-implemented referral credits probe."
    ui_start = patch.find(ui_start_marker)
    ui_end = patch.find(ui_end_marker, ui_start)
    if ui_start < 0 or ui_end < 0:
        raise RuntimeError("staged UI patch markers not found")
    return patch[:ui_start] + patch[ui_end:]


def replace_once(text: str, old: str, new: str, name: str) -> str:
    count = text.count(old)
    if count != 1:
        raise RuntimeError(f"{name}: expected 1 match, found {count}")
    return text.replace(old, new, 1)


def apply_usage_ui_patch() -> None:
    text = MAIN.read_text(encoding="utf-8")

    account_block = '''        <label>
          <span data-i18n="account.credential">Codex credential</span>
          <select id="account"></select>
        </label>'''
    text = replace_once(
        text,
        account_block,
        account_block
        + '''
        <label>
          <span data-i18n="account.cookie">Browser Cookie (optional)</span>
          <input id="cookie" type="password" autocomplete="off" spellcheck="false">
        </label>''',
        "cookie field",
    )
    text = replace_once(
        text,
        "        'account.credential': 'Codex credential',",
        "        'account.credential': 'Codex credential',\n        'account.cookie': 'Browser Cookie (optional)',",
        "English cookie translation",
    )
    text = replace_once(
        text,
        "        'account.credential': 'Codex 凭据',",
        "        'account.credential': 'Codex 凭据',\n        'account.cookie': '浏览器 Cookie（可选）',",
        "Chinese cookie translation",
    )
    text = replace_once(
        text,
        "    const accountSelect = document.getElementById('account');",
        "    const accountSelect = document.getElementById('account');\n    const cookieInput = document.getElementById('cookie');",
        "cookie DOM binding",
    )
    text = replace_once(
        text,
        '''          user_agent: settings.userAgent,
          management_origin: origin''',
        '''          user_agent: settings.userAgent,
          cookie: cookieInput.value.trim(),
          management_origin: origin''',
        "query Cookie payload",
    )
    text = replace_once(
        text,
        '''          auth_name: selected.dataset.name || '',
          management_origin: origin''',
        '''          auth_name: selected.dataset.name || '',
          cookie: cookieInput.value.trim(),
          management_origin: origin''',
        "redeem Cookie payload",
    )
    text = replace_once(
        text,
        '''      // Eligibility hit → remaining_reward_capacity is the "max rewards left" ceiling.
      if (d.eligibility_endpoint_hit && d.max_invites != null) {
        rows.push([t('metric.remainingReward'), fmtNumber(d.max_invites)]);
      }''',
        '''      if (d.eligibility_endpoint_hit && d.remaining_reward_capacity != null) {
        rows.push([t('metric.remainingReward'), fmtNumber(d.remaining_reward_capacity)]);
      }''',
        "reward-capacity rendering",
    )
    text = replace_once(
        text,
        '''      } else {
        rows.push([t('metric.source'), d.status_endpoint_hit ? t('source.status') : t('source.credits')]);
      }''',
        '''      } else if (d.status_endpoint_hit) {
        rows.push([t('metric.source'), t('source.status')]);
      } else {
        rows.push([t('metric.source'), d.referrals_credits_endpoint_hit ? t('source.credits') : t('source.usageFallback')]);
      }''',
        "referral source rendering",
    )

    MAIN.write_text(text, encoding="utf-8")


def main() -> None:
    patch = extract_staged_patch()
    namespace = {"__name__": "__main__"}
    exec(compile(patch, "<staged-review-fix>", "exec"), namespace, namespace)
    apply_usage_ui_patch()


if __name__ == "__main__":
    main()
