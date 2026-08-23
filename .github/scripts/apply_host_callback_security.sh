#!/usr/bin/env bash
set -euo pipefail

EXPECTED_HEAD="ci/apply-host-callback-security-20260823"
if [[ "${GITHUB_HEAD_REF:-}" != "${EXPECTED_HEAD}" ]]; then
  echo "Refusing one-shot patch outside ${EXPECTED_HEAD}" >&2
  exit 1
fi

git fetch origin main
git checkout -B host-callback-security-main origin/main

# The large generated Go blocks are kept as Python triple-quoted templates. Convert
# their raw-string prefixes in memory so \t and escaped quotes become valid Go source.
python - <<'PY'
from pathlib import Path

script_path = Path('.github/scripts/apply_host_callback_security.py')
source = script_path.read_text(encoding='utf-8')
replacements = {
    "accounts_block = r'''": "accounts_block = '''",
    "credential_block = r'''": "credential_block = '''",
    "origin_block = r'''": "origin_block = '''",
    'Path("host_security_test.go").write_text(\n    r\'\'\'package main': 'Path("host_security_test.go").write_text(\n    \'\'\'package main',
}
for old, new in replacements.items():
    count = source.count(old)
    if count != 1:
        raise SystemExit(f'template normalization expected one match, found {count}: {old}')
    source = source.replace(old, new, 1)
exec(compile(source, str(script_path), 'exec'), {'__name__': '__main__'})
PY

python - <<'PY'
from pathlib import Path

path = Path('Makefile')
text = path.read_text(encoding='utf-8')
old = '''test:
\t@if [ "$${GITHUB_HEAD_REF:-}" = "ci/apply-host-callback-security-20260823" ]; then bash .github/scripts/apply_host_callback_security.sh; fi
\tgo test ./...
'''
new = '''test:
\tgo test ./...
'''
count = text.count(old)
if count != 1:
    raise SystemExit(f'temporary Makefile hook: expected one match, found {count}')
path.write_text(text.replace(old, new, 1), encoding='utf-8', newline='\n')
PY

rm -f \
  .github/scripts/apply_host_callback_security.py \
  .github/scripts/apply_host_callback_security.sh

gofmt -w ./*.go
go test ./...
go vet ./...
git diff --check

git add -A
git diff --cached --check
git config user.name "github-actions[bot]"
git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
git commit -m "security: use host auth callbacks and restrict management fallback"
git push origin HEAD:main
