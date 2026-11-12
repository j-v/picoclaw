#!/usr/bin/env bash
# Tiered golangci-lint runner.
#
# Mirrors CI's build tags and behaviour, but lets you lint *locally* at whatever
# granularity your hardware can survive. On a memory-constrained box the caps
# are applied automatically; on a normal machine they are skipped, so this
# script is not Pi-specific.
#
# Usage:
#   scripts/lint.sh                 # packages changed vs BASE (default, fast)
#   scripts/lint.sh --all           # whole module (slow; matches CI scope)
#   scripts/lint.sh --ci            # alias for --all
#   scripts/lint.sh --fmt           # formatters only (very cheap, safe as a gate)
#   scripts/lint.sh --files a.go    # lint the packages owning specific files
#   scripts/lint.sh --warm          # populate caches; report but never fail
#   scripts/lint.sh --install-hooks # point git at .githooks/ (one-time setup)
#
# Environment:
#   LINT_BASE   git ref to diff against for changed-package mode
#               (default: first of fork/main, origin/main, upstream/main, main)
#   LINT_TAGS   build tags (default: goolm,stdjson -- must match CI)
#
# Why tiers: a cold full-module run on an SBC is measured in tens of minutes
# (dominated by one cgo translation unit, not by linting). A cold changed-package
# run is ~9 minutes; the same run warm is ~40 seconds. Gate on the cheap tier,
# let CI own the expensive one.
set -uo pipefail

REPO="${PICOCLAW_REPO:-$(git rev-parse --show-toplevel 2>/dev/null || echo "$PWD")}"
cd "$REPO" || { echo "lint: no repo at $REPO" >&2; exit 2; }

# shellcheck source=scripts/lib/lint-env.sh
. "$REPO/scripts/lib/lint-env.sh"

# Base ref for changed-package mode. A stale local branch silently inflates the
# diff (a `main` that is 35 commits behind relints 35 commits of unrelated work),
# so the default resolves to the freshest upstream-tracking branch rather than
# assuming "main" is current. Resolve after arg parsing so --files/--all skip it.
BASE="${LINT_BASE:-}"
TAGS="$(picoclaw_build_tags)"

# golangci-lint shells out to `go` for package loading and for gci/gomod_salt,
# and non-interactive shells often lack it on PATH even when installed.
picoclaw_resolve_go || true

# ---------------------------------------------------------------------------
# Mode + arg parsing
# ---------------------------------------------------------------------------
MODE=changed
TARGETS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --all|--ci)  MODE=all ;;
    --fmt)       MODE=fmt ;;
    --warm)      MODE=warm ;;
    --files)     MODE=files; shift ;;
    --install-hooks) MODE=install-hooks ;;
    -h|--help)   awk 'NR>1 && /^#/ {sub(/^# ?/,""); print; next} NR>1 {exit}' "$0"; exit 0 ;;
    -*)          echo "lint: unknown flag $1" >&2; exit 2 ;;
    *)           TARGETS+=("$1") ;;
  esac
  shift
done

if [ "$MODE" = "install-hooks" ]; then
  git config core.hooksPath .githooks
  echo "core.hooksPath -> .githooks"
  echo "hooks now active:"
  ls -1 .githooks 2>/dev/null | sed 's/^/  /'
  exit 0
fi

if [ "$MODE" = "files" ] || { [ "$MODE" = "fmt" ] && [ "${#TARGETS[@]}" -gt 0 ]; }; then
  SCOPE=""
  for t in "${TARGETS[@]}"; do
    d=$(dirname "$t")
    SCOPE="$SCOPE ./$d"
  done
elif [ "$MODE" = "all" ] || [ "$MODE" = "warm" ]; then
  SCOPE="./..."
else
  # Changed packages relative to the base branch, via the merge base so we don't
  # pick up unrelated drift. Falls back to a direct diff if no merge base exists.
  if [ -z "$BASE" ]; then
    # Prefer a remote-tracking tip: local branches go stale unnoticed, and a
    # stale base turns a 0-file diff into a 35-file diff.
    for cand in fork/main origin/main upstream/main main; do
      if git rev-parse --verify --quiet "$cand" >/dev/null 2>&1; then
        BASE="$cand"
        break
      fi
    done
    BASE="${BASE:-main}"
  fi

  if MB=$(git merge-base "$BASE" HEAD 2>/dev/null); then
    DIFF_BASE="$MB"
  else
    DIFF_BASE="$BASE"
  fi

  # If the base is a local branch that has fallen behind, say so: the scope is
  # still correct but much larger than the author expects. Check the branch's
  # own upstream first, then fork/main, since a local branch can track a
  # different remote than the one feature branches are cut from.
  case "$BASE" in
    */*) : ;;  # remote-tracking, assume fresh enough
    *)
      for up in "$(git rev-parse --abbrev-ref --symbolic-full-name "$BASE@{upstream}" 2>/dev/null || true)" fork/main; do
        [ -n "$up" ] || continue
        git rev-parse --verify --quiet "$up" >/dev/null 2>&1 || continue
        BEHIND=$(git rev-list --count "$BASE..$up" 2>/dev/null || echo 0)
        if [ "${BEHIND:-0}" -gt 0 ]; then
          echo "lint: WARNING base '$BASE' is $BEHIND commits behind '$up'."
          echo "lint:         scope will be inflated; fetch or set LINT_BASE=$up."
        fi
      done
      ;;
  esac
  CHANGED=$(git diff --name-only "$DIFF_BASE" -- '*.go' 2>/dev/null \
            | xargs -r -n1 dirname | sort -u | sed 's|^|./|' | tr '\n' ' ')
  if [ -z "$CHANGED" ]; then
    echo "lint: no Go changes vs $BASE — nothing to lint."
    exit 0
  fi
  SCOPE="$CHANGED"
fi

# ---------------------------------------------------------------------------
# Adaptive resource limits
#
# Only constrain when the machine actually needs it. Detection is by available
# RAM rather than by hostname so forks of this script behave sanely anywhere.
# ---------------------------------------------------------------------------
AVAIL_MB=$(awk '/^MemAvailable:/{print int($2/1024)}' /proc/meminfo 2>/dev/null || echo 4096)
LIMITS=()
CONC=""

if [ "$AVAIL_MB" -lt 2048 ]; then
  export GOMAXPROCS=1          # leave cores for interactive work
  export GOGC=50               # trade CPU for heap; halves growth
  export GOMEMLIMIT=512MiB     # hard ceiling: GC presses before the OOM killer
  CONC=1
  NICE_LEVEL=19
  IONICE_CLASS=3
  LIMITS+=("GOMAXPROCS=1" "GOGC=50" "GOMEMLIMIT=512MiB" "conc=1" "nice=$NICE_LEVEL")
  [ -n "${SUDO_USER:-}" ] && true
  if ! ionice -c3 true 2>/dev/null; then
    IONICE_CLASS=""
    LIMITS+=("ionice=unavailable")
  fi
else
  LIMITS+=("unconstrained (MemAvailable=${AVAIL_MB}M)")
fi

# Go writes compiler scratch to TMPDIR. If that is a small RAM-backed tmpfs the
# run dies partway with "no space left on device" while also consuming the RAM
# it was meant to save. Redirect scratch to disk when that is the case.
if [ -d "${TMPDIR:-/tmp}" ] && df -m "${TMPDIR:-/tmp}" 2>/dev/null | awk 'NR==2{exit !($2<1024)}'; then
  SCRATCH="${XDG_CACHE_HOME:-$HOME/.cache}/picoclaw-lint-tmp"
  mkdir -p "$SCRATCH"
  export TMPDIR="$SCRATCH"
  export GOTMPDIR="$SCRATCH"
  LIMITS+=("TMPDIR->$SCRATCH (tmpfs too small)")
fi

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
if ! command -v golangci-lint >/dev/null 2>&1; then
  echo "lint: golangci-lint not found on PATH." >&2
  echo "      CI pins v2.10.1; install that version so local matches CI:" >&2
  echo "      https://github.com/golangci/golangci-lint/releases/tag/v2.10.1" >&2
  exit 2
fi

if ! command -v go >/dev/null 2>&1; then
  echo "lint: go toolchain not found on PATH (golangci-lint needs it)." >&2
  echo "      Set PICOCLAW_GO_BIN=/path/to/go/bin or put go on PATH." >&2
  exit 2
fi

printf 'lint: mode=%s  scope=%s\n' "$MODE" "$SCOPE"
printf 'lint: %s\n' "${LIMITS[*]}"
printf 'lint: avail=%sM  build-cache=%sM\n' "$AVAIL_MB" \
  "$(du -sm "${XDG_CACHE_HOME:-$HOME/.cache}/go-build" 2>/dev/null | cut -f1 || echo 0)"

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
RUNNER=()
[ -n "${NICE_LEVEL:-}" ] && RUNNER=(nice -n "$NICE_LEVEL")
if [ -n "${IONICE_CLASS:-}" ]; then
  RUNNER=(ionice -c"$IONICE_CLASS" "${RUNNER[@]}")
fi

CONC_ARG=()
[ -n "$CONC" ] && CONC_ARG=(--concurrency="$CONC")

START=$(date +%s)
if [ "$MODE" = "fmt" ]; then
  "${RUNNER[@]}" golangci-lint fmt --diff $SCOPE
  RC=$?
else
  "${RUNNER[@]}" golangci-lint run \
    --build-tags="$TAGS" \
    --timeout=30m \
    "${CONC_ARG[@]}" \
    $SCOPE
  RC=$?
fi
END=$(date +%s)

printf -- '---\nlint: exit=%s  duration=%ss  avail=%sM\n' "$RC" "$((END - START))" \
  "$(awk '/^MemAvailable:/{print int($2/1024)}' /proc/meminfo 2>/dev/null || echo '?')"

# --warm exists to populate the cache after a `go clean`; its findings are
# informational because the point of running it is the side effect.
if [ "$MODE" = "warm" ] && [ "$RC" -ne 0 ]; then
  echo "lint: --warm run reported issues (not failing; cache is now populated)."
  exit 0
fi

exit "$RC"
