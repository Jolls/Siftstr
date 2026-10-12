#!/usr/bin/env bash
# Fails when a change touches behavior-affecting paths without a CHANGELOG.md entry.
# Usage: scripts/check-changelog.sh <base-ref> <head-ref>
# Env:   PR_LABELS  comma or newline separated label names; "skip-changelog" skips the check.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <base-ref> <head-ref>" >&2
  exit 2
fi
base=$1
head=$2

if printf '%s\n' "${PR_LABELS:-}" | tr ',' '\n' | grep -qx 'skip-changelog'; then
  echo "skip-changelog label present, skipping"
  exit 0
fi

files=$(git diff --name-only "$base...$head")

if printf '%s\n' "$files" | grep -qx 'CHANGELOG.md'; then
  echo "CHANGELOG.md is updated"
  exit 0
fi

# Behavior-affecting: code under cmd/, internal/, web/, plus deployment files.
# Exempt: tests, test data, the dev seeder.
needs=$(printf '%s\n' "$files" | grep -E '^(cmd/|internal/|web/)|^(Dockerfile|compose\.yaml|\.env\.example)$' \
  | grep -Ev '_test\.go$|/testdata/|^web/test/|^cmd/seed/' || true)

if [ -z "$needs" ]; then
  echo "no behavior-affecting paths changed"
  exit 0
fi

echo "These files affect behavior but CHANGELOG.md was not updated:"
printf '  %s\n' $needs
echo "Add an entry under [Unreleased] in CHANGELOG.md, or add the skip-changelog label."
exit 1
