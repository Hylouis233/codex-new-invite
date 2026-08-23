#!/usr/bin/env bash
set -euo pipefail

EXPECTED_HEAD="ci/apply-usage-ui-security-20260823"
if [[ "${GITHUB_HEAD_REF:-}" != "${EXPECTED_HEAD}" ]]; then
  echo "Refusing one-shot patch outside ${EXPECTED_HEAD}" >&2
  exit 1
fi

git fetch origin main
git checkout -B usage-ui-security-main origin/main
python .github/scripts/apply_usage_ui_security.py

python - <<'PY'
from pathlib import Path

path = Path("Makefile")
text = path.read_text(encoding="utf-8")
old = '''test:
\t@if [ "$${GITHUB_HEAD_REF:-}" = "ci/apply-usage-ui-security-20260823" ]; then bash .github/scripts/apply_usage_ui_security.sh; fi
\tgo test ./...
'''
new = '''test:
\tgo test ./...
'''
count = text.count(old)
if count != 1:
    raise SystemExit(f"temporary Makefile hook: expected one match, found {count}")
path.write_text(text.replace(old, new, 1), encoding="utf-8", newline="\n")
PY

rm -f \
  .github/workflows/security-sanitize-usage-ui.yml \
  .github/scripts/apply_usage_ui_security.py \
  .github/scripts/apply_usage_ui_security.sh

gofmt -w main.go security.go security_test.go ui_security_test.go
go test ./...
go vet ./...
git diff --check

git add -A
git diff --cached --check
git config user.name "github-actions[bot]"
git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
git commit -m "security: harden usage UI rendering and key handling"
git push origin HEAD:main
