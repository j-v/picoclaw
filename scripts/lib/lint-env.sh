#!/usr/bin/env bash
# Shared environment resolution for lint tooling. Source this; do not execute.
#
# Both scripts/lint.sh and .githooks/pre-commit need golangci-lint to find a Go
# toolchain, and non-interactive shells (git hooks in particular) routinely lack
# one on PATH even when it is installed. Keeping the search in one place means a
# new install location only has to be taught once.

# Put a Go toolchain on PATH if one is not already resolvable.
# Honours PICOCLAW_GO_BIN for explicit overrides.
picoclaw_resolve_go() {
  command -v go >/dev/null 2>&1 && return 0
  for cand in "${PICOCLAW_GO_BIN:-}" \
              "$HOME/go126/go/bin" "$HOME/go/bin" "$HOME/.local/go/bin" \
              /usr/local/go/bin /usr/lib/go/bin /snap/bin; do
    if [ -n "$cand" ] && [ -x "$cand/go" ]; then
      PATH="$cand:$PATH"; export PATH
      return 0
    fi
  done
  return 1
}

# Resolve gitleaks, preferring PATH then a user-local install.
picoclaw_resolve_gitleaks() {
  if command -v gitleaks >/dev/null 2>&1; then
    command -v gitleaks
  elif [ -x "$HOME/go/bin/gitleaks" ]; then
    echo "$HOME/go/bin/gitleaks"
  else
    return 1
  fi
}

# Build tags must match CI, or local results are not comparable.
picoclaw_build_tags() {
  echo "${LINT_TAGS:-goolm,stdjson}"
}
