#!/usr/bin/env bash
# ============================================================
# PicoClaw pipeline — one-time Pi bootstrap (interactive)
#
# Run ONCE from an interactive SSH session on the Pi:
#   cd ~/picoclaw && bash scripts/setup-picoclaw.sh
#
# It needs sudo (first TWO commands only), your GitHub login
# (gh auth login), and your hands (deploy key paste, autostart edit).
# After this step, everything else is automated.
#
# Not fully idempotent: if it fails midway, fix the failing step
# by hand and re-run.
# ============================================================
set -euo pipefail

PIPELINE_DIR="/opt/picoclaw"
RELEASES_DIR="$PIPELINE_DIR/releases"
STABLE_DIR="$PIPELINE_DIR/stable"
USER_NAME="$(whoami)"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"   # parent of scripts/ = repo root
LAUNCHER_SERVICE="$HOME/.config/systemd/user/picoclaw-launcher.service"
DEPLOY_SERVICE="$HOME/.config/systemd/user/picoclaw-deploy.service"
AUTOSTART="$HOME/.config/labwc/autostart"

echo "=== PicoClaw pipeline setup ==="
echo "Running as: $USER_NAME"
echo "Repo dir:   $REPO_DIR"
echo "Pipeline:   $PIPELINE_DIR"
echo ""

# ------------------------------------------------------------
# 0. Prerequisites + auth (the first of two sudo steps)
# ------------------------------------------------------------
echo "[0/8] Installing prerequisites (sudo apt)..."
sudo apt update
sudo apt install -y gh jq netcat-openbsd curl file

for cmd in gh jq nc curl file; do
  command -v "$cmd" >/dev/null 2>&1 || { echo "❌ Missing: $cmd — install it and re-run."; exit 1; }
done
echo "   ✔️  Prerequisites present: gh jq nc curl file"

if ! gh auth status >/dev/null 2>&1; then
  echo ""
  echo "--- gh auth login (interactive) ---"
  echo "Choose: GitHub.com → HTTPS → Login with a web browser → Yes (git credentials)"
  echo ""
  gh auth login
fi
echo "   ✔️  gh authenticated as: $(gh api user --jq .login 2>/dev/null || echo '?')"

# ------------------------------------------------------------
# 1. Create directory structure (the second and last sudo)
# ------------------------------------------------------------
echo ""
echo "[1/8] Creating $PIPELINE_DIR (sudo mkdir + chown)..."
sudo mkdir -p "$STABLE_DIR" "$RELEASES_DIR"
sudo chown -R "$USER_NAME:$USER_NAME" "$PIPELINE_DIR"
echo "   ✔️  $PIPELINE_DIR ready (owned by $USER_NAME)"

# ------------------------------------------------------------
# 2. Snapshot current release → stable/ (emergency fallback)
# ------------------------------------------------------------
echo ""
echo "[2/8] Snapshotting current release → $STABLE_DIR ..."
if [ -f /usr/local/bin/picoclaw ] && [ -f /usr/local/bin/picoclaw-launcher ]; then
  cp /usr/local/bin/picoclaw "$STABLE_DIR/"
  cp /usr/local/bin/picoclaw-launcher "$STABLE_DIR/"
else
  echo "   ⚠️  /usr/local/bin binaries not found — skipping stable snapshot."
fi
ln -sfn "$STABLE_DIR" "$PIPELINE_DIR/current"
ls -la "$STABLE_DIR/"
echo "   ✔️  stable snapshot + current symlink (→ stable)"

# ------------------------------------------------------------
# 3. Install the deploy watchdog script
# ------------------------------------------------------------
echo ""
echo "[3/8] Installing deploy-picoclaw.sh..."
cp "$REPO_DIR/deploy-picoclaw.sh" "$PIPELINE_DIR/deploy-picoclaw.sh"
chmod +x "$PIPELINE_DIR/deploy-picoclaw.sh"
echo "   ✔️  $PIPELINE_DIR/deploy-picoclaw.sh"

# ------------------------------------------------------------
# 4. Launcher user service
# ------------------------------------------------------------
echo ""
echo "[4/8] Writing launcher user service..."
mkdir -p "$HOME/.config/systemd/user"
cat > "$LAUNCHER_SERVICE" <<'EOS'
[Unit]
Description=PicoClaw gateway launcher
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/picoclaw/current/picoclaw-launcher -no-browser -public
# Always: even a clean exit comes back — the watchdog intentionally
# kills+restarts this during deploys, so this is safe (and desirable).
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOS
echo "   ✔️  $LAUNCHER_SERVICE"

# ------------------------------------------------------------
# 5. Deploy watchdog user service
# ------------------------------------------------------------
echo ""
echo "[5/8] Writing deploy watchdog user service..."
cat > "$DEPLOY_SERVICE" <<'EOS'
[Unit]
Description=PicoClaw deploy watchdog — polls GitHub, deploys on new builds
Documentation=https://github.com/j-v/picoclaw
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/picoclaw/deploy-picoclaw.sh
Restart=on-failure
RestartSec=10

# Rate-limit: 5 crashes in 10 minutes → give up and stop
# (prevents infinite crash-loop hammering the GitHub API while debugging)
StartLimitIntervalSec=600
StartLimitBurst=5

[Install]
WantedBy=default.target
EOS
echo "   ✔️  $DEPLOY_SERVICE"

# ------------------------------------------------------------
# 5b. Stop the old bare launcher (port handoff — REQUIRED)
# ------------------------------------------------------------
echo ""
echo "[5b/8] Stopping old bare launcher (port handoff :18800)..."
if pgrep -f "picoclaw-launcher" >/dev/null 2>&1; then
  pkill -f "picoclaw-launcher" || true
  sleep 1
fi
if ss -tln | grep -q ":18800"; then
  echo "   ⚠️  Port 18800 still busy — waiting 3s..."
  sleep 3
fi
if ss -tln | grep -q ":18800"; then
  echo "   ❌ Port 18800 still in use — resolve manually, then re-run."
  exit 1
else
  echo "   ✔️  Port 18800 free"
fi

# ------------------------------------------------------------
# 6. Enable services
# ------------------------------------------------------------
echo ""
echo "[6/8] Enabling services..."
systemctl --user daemon-reload
systemctl --user enable --now picoclaw-launcher.service
systemctl --user enable --now picoclaw-deploy.service
echo "   ✔️  picoclaw-launcher.service + picoclaw-deploy.service enabled"

# ------------------------------------------------------------
# 7. Remove launcher from labwc autostart
# ------------------------------------------------------------
echo ""
echo "[7/8] Removing launcher from labwc autostart..."
if [ -f "$AUTOSTART" ]; then
  if grep -q "picoclaw-launcher" "$AUTOSTART"; then
    sed -i 's|.*picoclaw-launcher.*|# removed by pipeline setup: picoclaw-launcher (now systemd-managed)|' "$AUTOSTART"
    echo "   ✔️  Commented out picoclaw-launcher in $AUTOSTART"
  else
    echo "   (no picoclaw-launcher line found — nothing to do)"
  fi
else
  echo "   (no autostart file at $AUTOSTART — nothing to do)"
fi

# ------------------------------------------------------------
# 8. Verify
# ------------------------------------------------------------
echo ""
echo "[8/8] Verifying..."
sleep 2
echo "--- launcher service ---"
systemctl --user status picoclaw-launcher.service --no-pager | head -5 || true
echo "--- deploy service ---"
systemctl --user status picoclaw-deploy.service --no-pager | head -5 || true
echo ""
if curl -sf -o /dev/null http://localhost:18800; then
  echo "✅ Launcher responding on :18800"
else
  echo "⚠️  Launcher not responding yet — check: journalctl --user -u picoclaw-launcher -n 50 --no-pager"
fi

echo ""
echo "=== Setup complete ==="
echo ""
echo "Remaining one-time steps:"
echo "  1. Deploy key (Phase 4) — generate if not already done:"
echo "     ssh-keygen -t ed25519 -f ~/.ssh/picoclaw-deploy -N \"\""
echo "     → add pub key: https://github.com/j-v/picoclaw/settings/keys (Allow write access)"
echo "     → then: git remote set-url origin git@github-picoclaw:j-v/picoclaw.git"
echo "  2. Gitleaks (Phase 3):"
echo "     go install github.com/gitleaks/gitleaks/v8@latest   (or download the release binary)"
echo "     export PATH=\$PATH:\$HOME/go/bin"
echo "  3. Watch the first deploy: journalctl --user -u picoclaw-deploy.service -f"
