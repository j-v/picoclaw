#!/usr/bin/env bash
set -euo pipefail

# ============================================================
# PicoClaw deploy watchdog (Pi 3B)
# Long-running poll-and-deploy loop. Runs as a user systemd service.
#   Idle:    poll GitHub every 60s for latest successful run
#   Tight:   poll every 5s while a build is in progress
#   Deploy:  download artifact → pre-flight → symlink swap → restart → health check
#   Rollback: new → previous → stable (0.3.1 known-good)
#   Force:   systemctl --user kill -s USR1 picoclaw-deploy.service
# ============================================================

# ============================================================
# Configuration
# ============================================================
REPO="j-v/picoclaw"
WORKFLOW="build-arm64.yml"
RELEASES_DIR="/opt/picoclaw/releases"
STABLE_DIR="/opt/picoclaw/stable"         # 🛟 Known-good fallback (never pruned)
CURRENT_LINK="/opt/picoclaw/current"       # 🔀 Symlink to active release
STATE_FILE="/opt/picoclaw/.last-deployed-run"
FAILED_STATE_FILE="/opt/picoclaw/.failed-runs"   # run-id + failure count — permanently skipped after threshold
MARK_FAILED_THRESHOLD=2        # retry-once: permanently skip only after this many consecutive failures
GH_ARTIFACT_NAME="picoclaw-linux-arm64"
SERVICE="picoclaw-launcher"               # user systemd service name (--user)

# Deploy notifications (Telegram) — best-effort, never blocks a deploy
SECURITY_FILE="$HOME/.picoclaw/.security.yml"   # bot tokens live here (channel_list.telegram.settings.token)
TELEGRAM_CHAT_ID="8707367919"

# Health check — dual verification
#   :18800 — launcher web console (HTTP)
#   :18790 — gateway native port (TCP)
HEALTH_CHECK_URL="http://localhost:18800"
GATEWAY_PORT=18790
HEALTH_RETRIES=20               # 20 × 2s ≈ 40s window — Pi 3B cold starts are slow on SD cards
HEALTH_RETRY_INTERVAL=2         # seconds between retries

# Download timeout — never let a hung network wedge the watchdog mid-deploy
DOWNLOAD_TIMEOUT=180            # seconds

# Prerequisite binaries the script depends on
REQUIRED_CMDS=(gh jq nc curl file)

# Poll intervals
IDLE_SLEEP=60               # seconds between polls when idle
TIGHT_SLEEP=5               # seconds between polls when build in progress

KEEP_RELEASES=5             # number of release dirs to retain

# ============================================================
# Helpers
# ============================================================
log() { echo "[$(date +%H:%M:%S)] $*"; }

# ── Telegram notification (deploy events) ─────────────────────────────
# Best-effort: failures here are logged, never fatal. Reads the bot token
# from ~/.picoclaw/.security.yml (same source the compaction hook uses).
notify() {
  local msg="$1"
  local token
  token=$(python3 -c "
import yaml
try:
    d = yaml.safe_load(open('$SECURITY_FILE'))
    print(d.get('channel_list', {}).get('telegram', {}).get('settings', {}).get('token', ''))
except Exception:
    pass
" 2>/dev/null)

  if [ -z "$token" ]; then
    log "   (notify skipped: no telegram token in $SECURITY_FILE)"
    return 0
  fi

  if ! curl -sf -o /dev/null -X POST "https://api.telegram.org/bot${token}/sendMessage" \
      --data-urlencode "chat_id=$TELEGRAM_CHAT_ID" \
      --data-urlencode "text=$msg" \
      --data-urlencode "disable_web_page_preview=true" 2>/dev/null; then
    log "   (notify failed — telegram unreachable)"
  fi
}

ensure_dirs() {
  mkdir -p "$RELEASES_DIR" "/opt/picoclaw"
}

# Fail fast if gh isn't authenticated
check_gh_auth() {
  if ! gh auth status 2>/dev/null; then
    log "❌ gh CLI not authenticated. Run setup-picoclaw.sh step 0 (fine-grained PAT), or: gh auth login"
    exit 1
  fi
}

# Fail fast if any prerequisite binary is missing (silent hangs are the worst)
check_prereqs() {
  local missing=0
  for cmd in "${REQUIRED_CMDS[@]}"; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      log "❌ Missing required command: $cmd"
      missing=1
    fi
  done
  if [ "$missing" -eq 1 ]; then
    log "   Install missing commands, then restart the watchdog."
    log "   Debian: sudo apt install gh jq netcat-openbsd curl file"
    exit 1
  fi
  log "   ✔️  Prerequisite commands present: ${REQUIRED_CMDS[*]}"
}

# Record a run failure. Runs are only permanently skipped after
# MARK_FAILED_THRESHOLD consecutive failures — a transient network blip
# gets a second chance on the next poll (retry-once).
mark_failed() {
  local run_id="$1"
  local count=1
  touch "$FAILED_STATE_FILE"

  if grep -q "^$run_id " "$FAILED_STATE_FILE"; then
    count=$(grep "^$run_id " "$FAILED_STATE_FILE" | awk '{print $2}')
    count=$((count + 1))
  fi

  grep -v "^$run_id " "$FAILED_STATE_FILE" > "${FAILED_STATE_FILE}.tmp" 2>/dev/null || true
  mv "${FAILED_STATE_FILE}.tmp" "$FAILED_STATE_FILE" 2>/dev/null || true
  echo "$run_id $count" >> "$FAILED_STATE_FILE"

  if [ "$count" -ge "$MARK_FAILED_THRESHOLD" ]; then
    log "   Marked run #$run_id as permanently failed ($count consecutive failures) — will not retry"
    notify "🚨 Deploy skipped: run #$run_id failed $count× in a row (artifact expired/corrupt) — will not retry"
  else
    log "   Run #$run_id failed (attempt $count/$MARK_FAILED_THRESHOLD) — will retry on next poll"
  fi
}

# True if the run has hit the permanent-failure threshold
is_failed() {
  local run_id="$1"
  [ -f "$FAILED_STATE_FILE" ] || return 1
  local count
  count=$(grep "^$run_id " "$FAILED_STATE_FILE" | awk '{print $2}')
  [ -n "$count" ] && [ "$count" -ge "$MARK_FAILED_THRESHOLD" ]
}

# Clear failure state after a successful deploy (no longer pending retry)
clear_failed() {
  local run_id="$1"
  [ -f "$FAILED_STATE_FILE" ] || return 0
  grep -v "^$run_id " "$FAILED_STATE_FILE" > "${FAILED_STATE_FILE}.tmp" 2>/dev/null || true
  mv "${FAILED_STATE_FILE}.tmp" "$FAILED_STATE_FILE" 2>/dev/null || true
}

# ============================================================
# Health check — verify launcher + gateway are both responsive
# ============================================================
check_health() {
  local max_attempts="$1"
  local attempt=0

  while [ "$attempt" -lt "$max_attempts" ]; do
    attempt=$((attempt + 1))

    # Check 1: Launcher process alive?
    if ! pgrep -f "picoclaw-launcher" > /dev/null; then
      sleep "$HEALTH_RETRY_INTERVAL"
      continue
    fi

    # Check 2: Launcher HTTP web UI responding?
    if curl -sf -o /dev/null "$HEALTH_CHECK_URL" 2>/dev/null; then
      # Check 3: Gateway accepting TCP connections on its native port?
      if nc -z localhost "$GATEWAY_PORT" 2>/dev/null; then
        return 0   # healthy!
      fi
    fi

    sleep "$HEALTH_RETRY_INTERVAL"
  done

  return 1   # unhealthy after all retries
}

# ============================================================
# Rollback — revert to previous release
# ============================================================
rollback() {
  local failed_release="$1"

  log "❌ Rolling back — removing failed release: $failed_release"

  # Find the previous release (by directory name, sorted)
  local prev
  prev=$(ls -1t "$RELEASES_DIR" 2>/dev/null | head -n 2 | tail -n 1)
  # Guard: if there's only one release dir (the failed one), there is no previous
  [ "$prev" = "$failed_release" ] && prev=""

  # --- Helper: activate a given release dir and health-check ---
  deploy_from() {
    local src_dir="$1"
    local label="$2"

    ln -sfn "$src_dir" "$CURRENT_LINK"

    log "   Restarting $SERVICE with $label..."
    pkill -f "picoclaw-launcher" 2>/dev/null || true   # clear strays (incl. old systemd instance) before restart
    systemctl --user restart "$SERVICE" 2>/dev/null || true

    if check_health "$HEALTH_RETRIES"; then
      log "✅ $label gateway is running"
      return 0
    fi
    return 1
  }

  # --- Attempt 1: rollback to previous release ---
  if [ -n "$prev" ] && [ -d "$RELEASES_DIR/$prev" ]; then
    log "   Rolling back: current → $prev"
    rm -rf "$RELEASES_DIR/$failed_release"

    if deploy_from "$RELEASES_DIR/$prev" "previous"; then
      notify "⚠️ Deploy rolled back to previous release ($prev) after health check failure"
      return 0
    fi

    log "⚠️  Previous release ALSO failed health check — cascading to stable..."
  else
    log "   No previous release directory found — cascading to stable..."
    rm -rf "$RELEASES_DIR/$failed_release"
  fi

  # --- Attempt 2: fall back to known-good stable ---
  if [ -f "$STABLE_DIR/picoclaw-launcher" ] && [ -f "$STABLE_DIR/picoclaw" ]; then
    log "   💣 Stable fallback: deploying from $STABLE_DIR"
    if deploy_from "$STABLE_DIR" "stable (known-good)"; then
      notify "🛟 Stable fallback active (0.3.1 known-good) — previous release also failed health check"
      return 0
    fi
    log "🚨 Stable fallback ALSO failed! Something is fundamentally wrong."
  else
    log "🚨 No stable fallback found at $STABLE_DIR — files missing!"
  fi

  log "   Manual intervention required."
  notify "🚨 EMERGENCY: all rollback layers failed (new → previous → stable). Manual intervention required on the Pi."
  return 1
}

# ============================================================
# Deploy
# ============================================================
deploy() {
  local run_id="$1"
  local run_sha="$2"

  log "=== Deploying run #$run_id ($(echo "$run_sha" | head -c 7)) ==="

  # --- Download artifact ---
  local tmp_dir
  tmp_dir=$(mktemp -d)

  log "   Downloading artifact..."
  if ! timeout "$DOWNLOAD_TIMEOUT" gh run download "$run_id" --repo "$REPO" --name "$GH_ARTIFACT_NAME" --dir "$tmp_dir" 2>/dev/null; then
    log "❌ Failed to download artifact for run #$run_id"
    log "   (Likely expired — artifact retention is 1 day. Marking as skipped.)"
    mark_failed "$run_id"
    rm -rf "$tmp_dir"
    return 1
  fi

  # --- Locate binaries (artifact may be a flat dir or nested in pkg/) ---
  local launcher_bin=""
  local cli_bin=""

  if [ -f "$tmp_dir/picoclaw-launcher" ]; then
    launcher_bin="$tmp_dir/picoclaw-launcher"
    cli_bin="$tmp_dir/picoclaw"
  elif [ -f "$tmp_dir/pkg/picoclaw-launcher" ]; then
    launcher_bin="$tmp_dir/pkg/picoclaw-launcher"
    cli_bin="$tmp_dir/pkg/picoclaw"
  else
    log "❌ Could not find picoclaw-launcher or picoclaw in artifact"
    log "   Contents: $(ls -la "$tmp_dir" 2>/dev/null)"
    rm -rf "$tmp_dir"
    return 1
  fi

  # --- Pre-flight check ---
  log "   Pre-flight: verifying binaries..."
  for bin in "$launcher_bin" "$cli_bin"; do
    local file_type
    file_type=$(file "$bin")
    if ! echo "$file_type" | grep -qi "ELF.*64-bit.*ARM.*aarch64"; then
      log "❌ Pre-flight failed — unexpected binary type for $(basename "$bin"):"
      log "   $file_type"
      log "   Expected: ELF 64-bit ARM aarch64"
      mark_failed "$run_id"
      rm -rf "$tmp_dir"
      return 1
    fi
  done
  log "   ✔️  Pre-flight check passed (both binaries are ARM64 ELF)"

  chmod +x "$launcher_bin" "$cli_bin"

  # --- Create release directory ---
  local timestamp
  timestamp=$(date +%Y%m%d-%H%M%S)
  local release_dir="$RELEASES_DIR/$timestamp"
  mkdir -p "$release_dir"

  log "   Deploying to $release_dir/"
  cp "$launcher_bin" "$release_dir/picoclaw-launcher"
  cp "$cli_bin" "$release_dir/picoclaw"

  # --- Deploy: swap symlink and restart ---
  log "   Swapping symlink: current → $timestamp"
  ln -sfn "$release_dir" "$CURRENT_LINK"

  log "   Restarting $SERVICE..."
  pkill -f "picoclaw-launcher" 2>/dev/null || true   # clear strays (incl. old systemd instance) before restart
  systemctl --user restart "$SERVICE" 2>/dev/null || true

  # --- Health check ---
  log "   ⏳ Health check (HTTP :18800 + TCP :18790)..."
  if check_health "$HEALTH_RETRIES"; then
    log "✅  All health checks passed"
  else
    log "❌  Health check failed — rolling back..."
    rollback "$timestamp"
    rm -rf "$tmp_dir"
    return 1
  fi

  # --- Record state ---
  echo "$run_id" > "$STATE_FILE"
  clear_failed "$run_id"
  notify "✅ Deploy succeeded — run #$run_id ($(echo "$run_sha" | head -c 7)) is live (release $timestamp)"

  # --- Prune old releases ---
  log "   Pruning old releases..."
  ls -1t "$RELEASES_DIR" | tail -n +$((KEEP_RELEASES + 1)) | while read old; do
    log "     Removing $old"
    rm -rf "$RELEASES_DIR/$old"
  done

  rm -rf "$tmp_dir"
  log "=== Done ==="
}

# ============================================================
# Tight-poll a single run until it completes
# ============================================================
tight_poll() {
  local run_id="$1"
  log "⏳ Build #$run_id in progress — tight-polling every ${TIGHT_SLEEP}s..."

  while true; do
    sleep "$TIGHT_SLEEP"

    local run_info status conclusion
    run_info=$(gh run view "$run_id" --repo "$REPO" --json status,conclusion 2>/dev/null || echo '{"status":"unknown"}')
    status=$(echo "$run_info" | jq -r '.status')
    conclusion=$(echo "$run_info" | jq -r '.conclusion // empty')

    if [ "$status" = "completed" ]; then
      if [ "$conclusion" = "success" ]; then
        log "✅ Build #$run_id completed successfully!"
        deploy "$run_id" ""
      elif [ "$conclusion" = "failure" ] || [ "$conclusion" = "cancelled" ]; then
        log "❌ Build #$run_id $conclusion — skipping deploy"
      else
        log "⚠️  Build #$run_id completed with conclusion '$conclusion' — skipping deploy"
      fi
      return
    fi

    # Also check: did a different run complete while we were watching?
    local latest_success
    latest_success=$(gh run list --repo "$REPO" --workflow "$WORKFLOW" --branch main --status success --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null)
    if [ -n "$latest_success" ] && [ "$latest_success" != "$run_id" ]; then
      if [ ! -f "$STATE_FILE" ] || [ "$(cat "$STATE_FILE")" != "$latest_success" ]; then
        log "⚠️  New successful run #$latest_success appeared while tracking #$run_id — deploying"
        deploy "$latest_success" ""
        return
      fi
    fi
  done
}

# ============================================================
# Main loop
# ============================================================
check_gh_auth
check_prereqs
ensure_dirs

log "🐚 PicoClaw deploy watchdog started"
log "   Repo: $REPO"
log "   Workflow: $WORKFLOW"
log "   Service: $SERVICE (user service)"
log "   Idle poll: ${IDLE_SLEEP}s"
log "   Tight poll: ${TIGHT_SLEEP}s"
log "   Health check: $HEALTH_CHECK_URL (HTTP) + localhost:$GATEWAY_PORT (TCP) (${HEALTH_RETRIES}x ${HEALTH_RETRY_INTERVAL}s)"
log "   Manual trigger: SIGUSR1 → immediate poll (systemctl --user kill -s USR1 picoclaw-deploy)"

# Force-poll on SIGUSR1: `systemctl --user kill -s USR1 picoclaw-deploy.service`
# wakes the idle sleep and runs an immediate poll cycle (no PID needed).
poll_now=0
trap 'poll_now=1' USR1

while true; do
  # --- Phase 1: Check latest successful run (safety net) ---
  success_json=$(gh run list \
    --repo "$REPO" \
    --workflow "$WORKFLOW" \
    --branch main \
    --status success \
    --json databaseId,headSha \
    --jq '.[0] // empty' 2>/dev/null)

  if [ -n "$success_json" ]; then
    success_id=$(echo "$success_json" | jq -r '.databaseId')
    success_sha=$(echo "$success_json" | jq -r '.headSha // ""')

    last_deployed=""
    [ -f "$STATE_FILE" ] && last_deployed=$(cat "$STATE_FILE")

    if [ "$last_deployed" != "$success_id" ] && ! is_failed "$success_id"; then
      deploy "$success_id" "$success_sha"
      continue  # re-check immediately after deploy
    fi
  fi

  # --- Phase 2: Check for in-progress (tight-poll if found) ---
  in_progress=$(gh run list \
    --repo "$REPO" \
    --workflow "$WORKFLOW" \
    --branch main \
    --status in_progress \
    --json databaseId \
    --jq '.[0].databaseId // empty' 2>/dev/null)

  if [ -z "$in_progress" ]; then
    # Also check queued/pending (build hasn't started running yet)
    in_progress=$(gh run list \
      --repo "$REPO" \
      --workflow "$WORKFLOW" \
      --branch main \
      --status queued \
      --json databaseId \
      --jq '.[0].databaseId // empty' 2>/dev/null)
  fi

  if [ -n "$in_progress" ]; then
    tight_poll "$in_progress"
    continue
  fi

  # --- Phase 3: Nothing happening, idle ---
  # Interruptible sleep: SIGUSR1 (force-poll) wakes it immediately.
  # `|| true` is REQUIRED: with `set -e`, an interrupted `wait` returns
  # 128+10 and would kill the watchdog instead of continuing the loop.
  poll_now=0
  sleep "$IDLE_SLEEP" &
  wait $! || true
  if [ "$poll_now" = 1 ]; then
    log "   ⚡ Force-poll (SIGUSR1) — checking now"
  fi
done
