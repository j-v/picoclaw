# PicoClaw Self-Deploy Pipeline — Complete Setup & Self-Authoring Guide (v3)

> **Who this is for:** you're setting up the pipeline for the first time, and
> you want the primary day-to-day path to be **PicoClaw authoring its own
> changes from chat** while you just review and merge. v2 was a setup guide
> with a separate operating section; this guide merges the two and makes the
> self-authoring loop — and the `/dev-cycle` skill — the center of gravity.
> If you only need a status checklist for an already-running pipeline, see
> `PIPELINE_USER_GUIDE.md` (v1).

---

## Table of contents

1. [What you're building](#1-what-youre-building)
2. [Architecture](#2-architecture)
3. [Before you start](#3-before-you-start)
4. [Setup walkthrough](#4-setup-walkthrough)
5. [The self-authoring loop (operating day to day)](#5-the-self-authoring-loop-operating-day-to-day)
6. [Troubleshooting common situations](#6-troubleshooting-common-situations)
7. [Security model](#7-security-model)
8. [Optional hardening](#8-optional-hardening)
9. [Reference](#9-reference)

---

## 1. What you're building

You have a small ARM64 Linux box (in our case a Raspberry Pi 3B) running
**PicoClaw** — a Go application made of two binaries:

- **`picoclaw`** — the CLI and the AI gateway (connects out to Discord,
  Telegram, etc.)
- **`picoclaw-launcher`** — the local web console that supervises the gateway

You want two things:

1. **A safe, free, hands-off deploy pipeline**: change the code, have it
   cross-compiled on GitHub's free runners, and automatically deployed to the
   Pi — without babysitting builds, without root access, without paying for CI,
   and with a human in the loop at exactly the right place.
2. **A self-authoring dev loop**: the AI assistant writes the code, pushes
   branches, and drives the whole cycle from chat. You say what you want
   changed; PicoClaw authors it, opens the PR, pings you, and deploys after
   you merge.

The pipeline:

```
 you: "ship the fix for the health-check race"   (chat, no code)
        │
        ▼
 PicoClaw (on the Pi) edits files, runs pre-push checks, commits,
   pushes a fix/... branch
        │
        ▼
 auto-pr.yml opens a PR as github-actions[bot] (~30 s) → Telegram "merge me" ping
        │
        ▼
 you merge the PR on GitHub (one tap, from your phone if you like)  ← the human gate
        │
        ▼
 GitHub Actions (free runner — public repos get unlimited minutes)
   cross-compiles BOTH binaries for linux/arm64
   → uploads them as a build artifact (kept 1 day)
        │
        ▼
 Pi watchdog (a user systemd service, polls GitHub every 60s)
   sees the new successful build → downloads artifact
   → verifies it's a real ARM64 binary → creates a timestamped release
   → swaps a symlink → restarts the launcher → health-checks
   → rolls back automatically if anything is wrong
        │
        ▼
  new version is live, you got a Telegram ✅, the loop is closed
```

The headline properties:

- **$0 and no root.** Everything runs as your normal user (`systemctl --user`
  services, `loginctl enable-linger`). The only `sudo` in the whole system is
  a one-time setup script — after that, deploys, restarts, and rollbacks are
  all user-level.
- **Pull-based.** GitHub never knows your Pi exists. No inbound ports, no IP,
  no hostname in the repo. The Pi polls out and pulls what it needs.
- **Self-healing.** A bad deploy is caught by health checks within ~40 seconds
  and rolled back to the previous release automatically.
- **Self-authoring.** The AI assistant writes code, runs checks, commits,
  pushes, and coordinates rollbacks — all from chat. It pings you at every
  step, so nothing happens silently.
- **Human gate on what runs.** You are the only one who can merge to `main`.
  The agent can push feature branches and open PRs, but the pipeline only
  deploys `main` — so nothing runs without your approval.
- **Public repo on purpose.** GitHub only *enforces* branch protection on
  public repos for free accounts. Public + branch protection is the control
  that makes the human gate real.

At the end of this guide you'll have: a repo with CI that builds your ARM64
binaries, a Pi that polls and self-deploys them, automatic rollback,
notifications, and a documented **self-authoring loop** you can trigger from
chat with `/use dev-cycle`.

---

## 2. Architecture

### 2.1 Components

| Component | Where it lives | What it does |
|-----------|----------------|--------------|
| **Repo** (`you/picoclaw`) | GitHub | Source of truth. The only thing the Pi ever pulls from. |
| **PicoClaw agent** | Pi (`~/.picoclaw`, reachable via Discord/Telegram) | Authors code changes, runs pre-push checks, commits, pushes branches, monitors CI, reports status, coordinates rollbacks. |
| **Build workflow** (`build-arm64.yml`) | GitHub Actions | Cross-compiles both binaries for `linux/arm64`, packages them, uploads an artifact. Runs on every push (paths-filtered) and on `workflow_dispatch`. Artifact upload only happens for `main`. |
| **Artifact store** | GitHub Actions | Free temporary hosting (~62 MB per artifact, 1-day retention). The bridge between "GitHub built it" and "Pi deployed it". |
| **Auto-PR workflow** (`auto-pr.yml`) | GitHub Actions | When a `fix/**`, `feat/**`, or `feature/**` branch is pushed, opens a PR as `github-actions[bot]`. This is what keeps your AI agent from needing PR permissions. |
| **Secret-scan workflow** (`gitleaks.yml`) | GitHub Actions | Runs gitleaks on GitHub's servers (not the Pi) for every branch push. A required check on `main`. |
| **Dev-cycle skill** (`skills/dev-cycle/SKILL.md`) | repo + workspace | The agent's codified knowledge of the whole loop — how to commit, push, monitor CI, open/close PRs, and roll back. This is what `/use dev-cycle` engages. |
| **Watchdog** (`deploy-picoclaw.sh`) | Pi: `/opt/picoclaw/` | The long-running loop. Polls GitHub, downloads artifacts, pre-flights binaries, deploys, health-checks, rolls back. |
| **Launcher service** (`picoclaw-launcher.service`) | Pi: user systemd | Runs the web console; spawns the gateway. `ExecStart` points at a symlink so deploys are just symlink swaps. |
| **Watchdog service** (`picoclaw-deploy.service`) | Pi: user systemd | Runs the watchdog script; restarts it if it crashes. |
| **Log rotation** (`picoclaw-logrotate.timer`) | Pi: user systemd | Hourly timer that keeps `gateway.log`/`launcher.log` from growing forever. |

### 2.2 The journey of a change (self-authoring edition)

1. **You ask in chat** — "ship the fix for X", "make the deploy script do Y".
   PicoClaw engages the `/dev-cycle` skill and pings you at every step.
2. **Code change** — the agent edits the source on the Pi and commits to a
   **feature branch** (`fix/...`, `feat/...`). A pre-push checklist runs first:
   status, secrets, debug code, config sanity, commit message, branch name.
3. **Push** — the agent pushes the branch. The `auto-pr` workflow opens a PR
   as `github-actions[bot]` within ~30 seconds. The agent pings you on
   Telegram with the PR link — that's your "merge me" tap target.
4. **CI** — the build workflow cross-compiles both binaries for ARM64. The
   gitleaks workflow scans for secrets. Both must pass (branch protection).
5. **Merge (the human gate)** — *you* review and merge the PR, usually one tap
   on GitHub Mobile. Direct pushes to `main` are rejected by branch
   protection, even for admins.
6. **Build** — the merge triggers a fresh build on `main`. The runner
   cross-compiles and uploads the artifact (~1–2 min). **If the change touched
   nothing build-relevant** (docs, scripts, examples, assets only), the build
   steps are skipped entirely — the run still completes green (so branch
   protection stays satisfied) but uploads no artifact, and the watchdog
   records it in `.skipped-runs` instead of deploying.
7. **Watchdog detects** — the Pi's watchdog sees the build go `in_progress`
   and tight-polls every 5 s until it completes; or catches it on the next
   60 s idle poll if it finished in between. For a no-op run it sends a ℹ️
   "skipped" Telegram notice and moves on — no deploy.
8. **Deploy** — watchdog downloads the artifact, verifies both files are
   genuine `ELF 64-bit ARM aarch64` binaries, writes them into
   `/opt/picoclaw/releases/<timestamp>/`, points the `current` symlink at the
   new release, and restarts the launcher (which respawns the gateway).
9. **Health check** — for up to ~40 s it checks HTTP on `:18800` (launcher)
   and TCP on `:18790` (gateway). Both must pass.
10. **Done** — success is recorded (`.last-deployed-run`), old releases are
    pruned (keep 5), and a ✅ Telegram notification is sent. The agent pings
    you: "Step 7/7 — done, running release <timestamp>".

From merge to live: typically **1–3 minutes**.

### 2.3 Release layout on the Pi

```
/opt/picoclaw/                    ← owned by your user, no root anywhere
├── current → releases/20260803-093020/   ← symlink; services exec through it
├── stable/                        ← never-touched known-good snapshot (🛟)
│   ├── picoclaw
│   └── picoclaw-launcher
├── releases/
│   ├── 20260730-081500/           ← every deploy gets a timestamped dir
│   │   ├── picoclaw
│   │   └── picoclaw-launcher
│   ├── 20260803-093020/
│   └── 20260803-182810/           ← newest
├── deploy-picoclaw.sh             ← the watchdog script
├── .last-deployed-run             ← run ID of the last successful deploy
├── .skipped-runs                  ← runs with no build-relevant change (see §2.4)
└── .failed-runs                   ← run IDs that failed (see retry policy)
```

Why a symlink? Because systemd services `ExecStart` a *path*. If that path is
a symlink, a deploy is just: swap symlink → `systemctl --user restart
picoclaw-launcher`. No copying into system directories, no `sudo`, no service
file edits.

The launcher finds the gateway by looking in its **own directory** first, so
keeping both binaries in the same release dir means the launcher always
spawns the matching gateway version.

### 2.4 Ports, health checks, state

| Thing | Value |
|-------|-------|
| Launcher web console | `:18800` HTTP (health-check URL) |
| Gateway native port | `:18790` TCP (`nc -z localhost 18790`) |
| Health check window | 20 attempts × 2 s ≈ 40 s |
| Watchdog idle poll | every 60 s |
| Watchdog tight poll | every 5 s while a build is in progress |
| Artifact retention | 1 day |
| Releases kept | 5 (oldest pruned after each deploy) |
| Failed-run policy | retry **once**; after 2 consecutive failures the run is permanently skipped (🚨) |
| No-op (skip) policy | A successful run with **no build-relevant change** (e.g. docs, scripts, examples only) is recorded in `.skipped-runs` and skipped — no release, no deploy, just a ℹ️ Telegram notice. Idempotent: it won't re-notify on later polls. |

### 2.5 Logging layout

| Log | Where | How to read it |
|-----|-------|----------------|
| Watchdog / deploy script | stdout of `picoclaw-deploy.service` → system journal | `journalctl _SYSTEMD_USER_UNIT=picoclaw-deploy.service -f` |
| Launcher | `~/.picoclaw/logs/launcher.log` (JSONL file) | `tail -f ~/.picoclaw/logs/launcher.log` |
| Gateway | `~/.picoclaw/logs/gateway.log` (JSONL file) | `tail -f ~/.picoclaw/logs/gateway.log` |

Two things to know up front:

- The launcher/gateway write **files**, not stdout — so journald never sees
  them. That's by design (the Go logger opens with `O_APPEND`, which also
  makes `copytruncate` log rotation safe).
- `journalctl --user` may print *"No journal files were found"* on newer
  Debian (systemd quirk: per-user journal sockets never get created). The
  watchdog's stdout still lands in the **system** journal — use the
  `_SYSTEMD_USER_UNIT=` form above. This is cosmetic; the pipeline doesn't
  depend on `journalctl --user`.

Log growth is capped by user-level `logrotate` (hourly timer): `daily`,
`rotate 7`, `maxsize 20M`, `compress`, `delaycompress`, `copytruncate`.
Worst-case disk ≈ 35 MB, down from unbounded (~4.5 MB/day unrotated).

### 2.6 The rollback cascade

If a deployed release fails its health check, the watchdog walks down a
ladder until something works:

```
new release fails health check
   → remove the failed release dir
   → point current at the PREVIOUS release, restart, re-check
        → if that also fails…
   → point current at stable/ (the original known-good snapshot), restart, re-check
        → if that ALSO fails…
   → 🚨 emergency alert + Telegram; manual intervention
```

Every rung is logged with a distinct emoji marker (`❌ rolling back`,
`🛟 stable fallback`, `🚨 emergency`) and sends a Telegram notification, so a
bad deploy is loud, not silent. The agent watches this from chat and reports.

### 2.7 Why it's shaped this way (design decisions)

| Decision | Reason |
|----------|--------|
| Public repo | GitHub free plan only *enforces* branch protection on public repos. That enforcement is the hard human gate. |
| Pull-based Pi | No inbound ports, no SSH from GitHub, nothing for an attacker to probe. The Pi is just another GitHub API client. |
| User systemd services | No sudo for the 99.9% case. `loginctl enable-linger` keeps them running at boot. |
| Symlink + timestamped releases | Atomic-ish deploys, trivial rollback, and you can always see what *was* running. |
| Auto-PR as `github-actions[bot]` | The AI assistant's token needs **zero** PR permissions. The bot opens PRs; only a human can merge. |
| gitleaks on GitHub, not the Pi | The merge gate must run somewhere the Pi/agent can't fake. |
| Exec guard with `git push` allow-listed | The agent can push branches (that's the self-authoring loop) but still can't run `sudo`, `pkill`, `ssh host@`, heredocs, `${...}`/`$(...)`, or numeric `chmod`. |
| Telegram notifications | Deploys, rollbacks, and emergencies are visible instantly — an unauthorized deploy can't hide. |

---

## 3. Before you start

### 3.1 What you need

| Thing | Requirement | Notes |
|-------|-------------|-------|
| A Pi / ARM64 Linux box | `aarch64`, systemd, Debian-family recommended | The reference setup is a Pi 3B running Debian 13 (trixie). 1 GB RAM is fine. |
| `loginctl enable-linger` for your user | on | Needed so user services survive reboot. `loginctl enable-linger <user>` (one-time, needs sudo). |
| A GitHub account | Free plan is fine | The whole design targets the free tier. |
| A repo | Public, containing the PicoClaw source (or your fork) | Forks of `sipeed/picoclaw` work; keep upstream's other workflows — the pipeline is additive. |
| Chat access to the agent | Discord and/or Telegram | This is your *control surface*: you ask for changes in chat, the agent pings you at every step, and Telegram delivers the "merge me" PR pings. |
| Telegram bot token + chat ID | Recommended | Deploy notifications. Token goes in `~/.picoclaw/.security.yml` on the Pi. |
| GitHub Mobile | Recommended | One-tap PR merges from your phone (the human gate, made lazy-friendly). |
| Time | ~1 hour | Plus ~15 min of optional failure drills later. |

### 3.2 Tools installed on the Pi (during setup)

`gh` (GitHub CLI), `jq`, `netcat-openbsd`, `curl`, `file` — the watchdog
depends on all of them. The setup script installs them.

---

## 4. Setup walkthrough

The setup is deliberately **two-sided**: GitHub settings (browser, once) and
Pi bootstrap (a script, once). After that the loop is automated — and from
day two, driven from chat.

### Step 0 — Put the pipeline files in your repo

These files live in the repo so they're versioned, reviewable, and deployed
through the same pipeline they implement:

| File | Job |
|------|-----|
| `.github/workflows/build-arm64.yml` | Cross-compile + artifact upload |
| `.github/workflows/gitleaks.yml` | Server-side secret scan (required check) |
| `.github/workflows/auto-pr.yml` | Auto-open PRs on feature-branch push |
| `scripts/setup-picoclaw.sh` | One-time Pi bootstrap (run by hand once) |
| `deploy-picoclaw.sh` | The watchdog script (copied to `/opt/picoclaw/`) |
| `skills/dev-cycle/SKILL.md` | **Agent knowledge of the whole loop** — this is what makes self-authoring work |

The reference copies live in `j-v/picoclaw`. If you're building on a fork of
`sipeed/picoclaw`, add these files without touching upstream's `build.yml`,
`pr.yml`, etc. — the pipeline is additive by design.

A few notes on the build workflow, because the details matter:

- It builds with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`, tags
  `goolm,stdjson`, and embeds version/commit/build-time into the binaries.
- Artifact packaging goes into **`dist/`**, not `pkg/` — the repo's Go source
  already lives in top-level `pkg/`, and uploading that path would ship the
  whole source tree and break `gh run download` on the Pi.
- Artifact upload is gated on `github.ref == 'refs/heads/main'` — feature
  branches still get CI checks, but only `main` builds reach the watchdog.
- The workflow triggers on `push` (all branches), *not* `pull_request`,
  because PRs are opened by `github-actions[bot]` and GitHub gates
  bot-authored PR runs behind "approval required" in public repos. Push
  events from trusted collaborators run ungated, and their check results
  still show on the PR.

### Step 1 — GitHub settings (browser, ~10 minutes)

These are the only browser actions in the whole setup.

**1.1 Make the repo public** — Settings → General → Danger Zone → Change
visibility → Make public.

> Why: on the Free plan, GitHub only *enforces* branch protection rules on
> public repos. On private repos the rules are advisory until you pay for
> Team/Enterprise. Public + branch protection is the control that makes the
> "only you can merge to main" guarantee real. The leak-protection layers in
> Step 7 are therefore mandatory, not optional.

**1.2 Enable push protection** — Settings → Code security and analysis →
Push protection → Enable. Free for all repositories; blocks known secret
types from ever landing on any branch.

**1.3 Branch protection on `main`** — Settings → Branches → main → Edit:

- ✅ Require a pull request before merging
- ✅ Require status checks to pass → select **`gitleaks`** and
  **`Cross-Compile (linux/arm64)`** (those exact check names — not `build`,
  which is upstream's check and doesn't run on PR paths)
- ✅ Require branches to be up to date
- ✅ Dismiss stale approvals on new commits
- ✅ Include administrators

Verification: `git push origin main` fails with "protected branch" once
this is on — even for you, even for the Pi's deploy key.

**1.4 Allow Actions to open PRs** — Settings → Actions → General → Workflow
permissions → enable **"Allow GitHub Actions to create and approve pull
requests"**.

> This is the one setting people forget. The `permissions:` block in
> `auto-pr.yml` is *necessary but not sufficient*; without this repo-level
> toggle, `GITHUB_TOKEN` fails with *"GitHub Actions is not permitted to
> create or approve pull requests"*.

**1.5 (Later) Add the deploy key** — Settings → Deploy keys → Add deploy key.
You'll paste the public key generated in Step 5. Title it something like
`Pi 3B deploy` and ✅ Allow write access. This key is scoped to exactly this
repo.

### Step 2 — Pi prerequisites + GitHub token (SSH, ~5 minutes)

SSH into the Pi and install the tools the pipeline needs:

```bash
sudo apt update
sudo apt install -y gh jq netcat-openbsd curl file
```

Then create a **fine-grained personal access token** for `gh`:

- GitHub → Settings → Developer settings → Fine-grained tokens → Generate
- Only select repositories → *your* repo
- **Actions: Read-only**
- **Contents: Read and write**
- **Pull requests: leave UNSET** (the auto-pr workflow opens PRs; this token
  must never be able to create/approve/merge them — that's what makes the
  self-authoring loop safe)
- Expiration: 90 days (or shorter)

**Paste the token into the setup script when it prompts.** The script
authenticates with `gh auth login --hostname github.com --git-protocol https
--with-token`, reading your paste from stdin — the token is never written to a
file on disk (it ends up in `~/.config/gh/`, gh's normal credentials store).
There is deliberately **no browser `gh auth login`**: its full-account OAuth
scopes would contradict the least-privilege model.

> Run the script from a real SSH session (a TTY) so it can prompt. If there's
> no TTY (piped stdin, fully automated run), the script instead reads
> `~/.picoclaw/gh-token.txt` — create that file (one line, the token) and re-run.

### Step 3 — Run the setup script (the one interactive moment)

```bash
cd ~/src/picoclaw
bash scripts/setup-picoclaw.sh
```

The script does, in order:

| # | Action |
|---|--------|
| 0 | Installs prereqs, authenticates gh with the fine-grained PAT via **interactive `--with-token` prompt** (or `~/.picoclaw/gh-token.txt` when there's no TTY — deletes the file after), verifies `gh auth status`, soft-checks Actions: read |
| 1 | Creates `/opt/picoclaw/{stable,releases}` and `chown`s it to your user |
| 2 | Snapshots the currently-running binaries into `/opt/picoclaw/stable/` — the never-touched 🛟 fallback |
| 2b | **Points the CLI at the pipeline** (final `sudo`): `/usr/local/bin/picoclaw` → `/opt/picoclaw/current/picoclaw` (and same for `picoclaw-launcher`). `current` is swapped by the watchdog on every deploy/rollback, so this one-time symlink always resolves to the latest release — no per-deploy maintenance |
| 3 | Copies `deploy-picoclaw.sh` into `/opt/picoclaw/` |
| 4 | Writes `~/.config/systemd/user/picoclaw-launcher.service` |
| 5 | Writes `~/.config/systemd/user/picoclaw-deploy.service` |
| 5b | **Stops the old launcher** (port handoff — required, or the new service crashes on `:18800` already in use). It asks before killing; say yes. |
| 6 | `systemctl --user enable --now` both services |
| 7 | Removes the launcher line from `~/.config/labwc/autostart` (systemd owns it now) |
| 9 | Sends a **test Telegram notification** — expect "✅ PicoClaw pipeline initialized" |

> Heads-up: step 5b briefly restarts your gateway, so your chat connection
> may drop for a few seconds. That's expected — it comes back under systemd.

Verify after the script:

```bash
systemctl --user status picoclaw-launcher.service   # active (running)
systemctl --user status picoclaw-deploy.service     # active (running)
curl -sf http://localhost:18800                     # launcher responds
nc -z localhost 18790 && echo "gateway open"        # gateway accepts
```

### Step 4 — Gitleaks local hook (fast feedback, ~5 minutes)

The CI gitleaks check is the enforcement layer; a **local** pre-commit hook
gives you instant feedback while the agent commits. Install the pinned
version — this matters on ARM:

```bash
# ⚠️ Pin v8.21.2 — later releases use a WASM regex engine that is
# pathologically slow on ARM (34s+ per commit vs ~0.5s).
mkdir -p /tmp/gitleaks-install && cd /tmp/gitleaks-install
curl -sLO https://github.com/gitleaks/gitleaks/releases/download/v8.21.2/gitleaks_8.21.2_linux_arm64.tar.gz
tar -xzf gitleaks_8.21.2_linux_arm64.tar.gz
mkdir -p ~/go/bin && mv gitleaks ~/go/bin/gitleaks
chmod u+rwx,go+rx ~/go/bin/gitleaks
gitleaks version   # → 8.21.2
```

Add `~/go/bin` to your PATH, then install the pre-commit hook (it's
documented in `PIPELINE_PLAN.md` Step 7 — a small script that runs
`gitleaks protect --staged` on every commit and blocks on findings).

> Honest scope: a local hook is convenience, not a security boundary — anyone
> with shell access can `git commit --no-verify`. The *real* controls are
> GitHub push protection + the required CI check, which the Pi can't fake.

### Step 5 — Deploy key + git remote (~5 minutes)

The Pi (and the agent, through it) needs to push feature branches back to
GitHub:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/picoclaw-deploy -N ""
```

Paste `~/.ssh/picoclaw-deploy.pub` into GitHub → Settings → Deploy keys
(Step 1.5), with write access. Then point SSH at it for this one repo:

```bash
cat >> ~/.ssh/config << 'EOF'
Host github-picoclaw
    HostName github.com
    IdentityFile ~/.ssh/picoclaw-deploy
    IdentitiesOnly yes
EOF

cd ~/src/picoclaw
git remote add fork git@github-picoclaw:you/picoclaw.git
ssh -T git@github-picoclaw   # → "Hi you/picoclaw! You've successfully authenticated..."
```

This key is scoped to exactly one repo — even a fully compromised Pi can only
touch that repo. The agent's `git push` (allow-listed in its exec guard) uses
this remote.

### Step 6 — First deploy (the moment of truth)

You already have a CI build from when the pipeline files merged. The watchdog
polls every 60 s and should find it and deploy:

```bash
journalctl _SYSTEMD_USER_UNIT=picoclaw-deploy.service -f
```

Watch for the sequence: *new build found → downloading → pre-flight OK →
symlink swap → restart → health check → ✅ deploy succeeded*. Then verify:

```bash
readlink /opt/picoclaw/current          # → /opt/picoclaw/releases/<timestamp>
cat /opt/picoclaw/.last-deployed-run    # run ID matches the CI run
curl -sf http://localhost:18800         # launcher responds
nc -z localhost 18790 && echo gateway open
```

And you should have a ✅ Telegram notification. If the first deploy happens
to roll back, don't panic — that's the system working; check the watchdog log
for which health check failed.

### Step 7 — Day-2: smoke-test the self-authoring loop from chat

This is where it comes together. Install GitHub Mobile, sign in, then from
your chat window:

```
you:  /use dev-cycle "make a trivial doc tweak to the README"
```

Watch the agent: edit → pre-push checks → commit → push → auto-pr opens the
PR → Telegram "merge me" ping → you tap merge → watchdog deploys → ✅ ping
back in chat. That's the entire self-authoring loop, end to end, on day two.
This is also your first failure "drill" (see Section 8).

---

## 5. The self-authoring loop (operating day to day)

### 5.1 The mental model: you talk, PicoClaw codes, the pipeline ships

The whole thing is a conversation loop with three players:

| Player | What they do |
|--------|--------------|
| **You** (chat) | Say what you want changed. Review. Approve. Merge. Say "roll it back" when needed. |
| **PicoClaw** (the agent) | Listens for change requests, authors the code, runs pre-push checks, commits, pushes the branch, monitors CI, reports status, coordinates rollbacks. |
| **Automation** (workflows + watchdog) | `auto-pr.yml` opens the PR the moment a branch is pushed. CI builds/gitleaks run. The deploy watchdog on the Pi polls, deploys merges to main, health-checks, and auto-rolls-back. |

The key asymmetry: **PicoClaw does everything except the two human-only steps —
approving and merging.** That's deliberate (your fine-grained PAT has no
Pull-requests permission, so the agent physically cannot merge its own PRs).¹

> ¹ **Footnote — authoring from another machine.** Everything above assumes
> PicoClaw writes the code and pushes from the Pi. You *can* also write and
> push from a laptop/desktop — the pipeline doesn't care where a push comes
> from: `auto-pr.yml` still opens the PR for any `fix/**`/`feat/**`/`feature/**`
> branch, CI still runs, merge and deploy are identical. What you lose is the
> in-chat authoring, the step pings, and the Telegram "merge me" ping (the
> agent only notifies for its own pushes). That's a supported but **secondary**
> path — this guide is about the primary one: PicoClaw authoring from chat.

### 5.2 Quick start: the `/dev-cycle` skill

The dev cycle is codified as a skill. Two ways to engage it:

```
/use dev-cycle "ship the fix for the deploy health check"
```

or just ask in plain language — the skill description matches on keywords
like *change, commit, push, build, deploy, PR, rollback, revert*:

```
you:  can you make the deploy script send a Telegram ping when CI fails?
```

When the skill is active, PicoClaw runs the **7-step loop** and **pings you at
every step**, so you always know where the change is in the pipeline. No silent
phases.

| Command | Effect |
|---------|--------|
| `/use dev-cycle <message>` | Run the dev cycle for this one request |
| `/use dev-cycle` | Arm the skill for your *next* message |
| `/use clear` | Cancel an armed skill |
| plain chat | Natural-language match against the skill description |

### 5.3 The 7-step loop (what you'll see in chat)

Every change you ask for goes through this. The pings make it feel like
watching a build pipeline in a chat window:

```
1. ✏️  Make code changes (edit files via agent tools)
     → ping: "Step 1/7 — editing files"
2. 🔍 Pre-push checklist (status, secrets, debug code, config, commit message)
     → ping: "Step 2/7 — pre-push checks: ✅ passed / ❌ blocked"
3. ✅ Git commit (gitleaks pre-commit hook fires)
     → ping: "Step 3/7 — committed <sha>"
4. 🚀 Git push to feature branch → auto-pr opens the PR
     → ping: "Step 4/7 — pushed <branch>, PR #<n>: <url>"
     → Telegram ping with the PR link ("merge me" 📱)
5. 👀 Build monitoring (watchdog tight-polls; agent reports CI status)
     → ping: "Step 5/7 — CI: ✅ passed / ❌ failed"
6. ✅ Automated deploy — happens AFTER YOU MERGE
     → ping: "Step 6/7 — deploy started / rolled back"
7. 🏥 Post-deploy verification (launcher :18800 + gateway :18790)
     → ping: "Step 7/7 — ✅ done, running release <timestamp>"
```

**Your only required action in the whole loop:** approve + merge the PR
(step 4→5). Everything else is hands-off.

### 5.4 Self-authoring: what a change request looks like

PicoClaw authors changes *in its own repo* — `~/src/picoclaw` on the Pi. You
don't write code, you describe intent. A few real examples:

**Example A — a bug fix**

```
you:  the deploy watchdog re-deployed a permanently-failed run once.
      add a guard so tight_poll() skips runs already in .failed-runs.
```

What happens next (abridged):

```
🦞 Step 1/7 — editing files
   (agent edits /opt/picoclaw/deploy-picoclaw.sh → repo copy, adds
    && ! is_failed "$latest_success" to the tight-poll redirect)
🦞 Step 2/7 — pre-push checks: ✅ passed
🦞 Step 3/7 — committed cf88e091
🦞 Step 4/7 — pushed fix/deploy-tightpoll-is-failed-guard, PR #11
   📱 Telegram: "🦞 PR #11 ready — https://github.com/j-v/picoclaw/pull/11"
🦞 Step 5/7 — CI: ✅ passed (Cross-Compile + gitleaks green)
   (you tap merge on GitHub Mobile)
🦞 Step 6/7 — watchdog deployed release 20260803-093020
🦞 Step 7/7 — ✅ done, running release 20260803-093020, gateway healthy
```

**Example B — a config/documentation change**

```
you:  make setup-picoclaw.sh accept the GitHub token via an interactive
      gh auth login --with-token prompt instead of requiring a file.
```

Same loop, same pings. The agent edits the script, commits to a `fix/` branch,
pushes, the PR auto-opens, you get pinged on Telegram, you merge.

**Example C — a rollback**

```
you:  the new release is misbehaving, roll back to the previous one.
```

The agent uses the rollback procedure (see §5.6) — picks the previous release,
flips the symlink, restarts the launcher, and verifies. All from chat.

### 5.5 What the agent checks before it ever pushes

Every commit goes through the **pre-push checklist** (step 2). This is the
quality gate — the agent runs it itself, but it's worth knowing what's inside:

| Check | What it verifies |
|-------|-----------------|
| `git status` | Only intended files changed |
| **No secrets** | Gitleaks pre-commit hook + manual scan — no API keys, tokens, or `.security.yml` references |
| **No debug code** | No `console.log`, `fmt.Println`, `debug.*`, leftover TODOs |
| **Config sanity** | `config.json`/YAML changes are intentional, no hardcoded creds |
| **Commit message** | Conventional format (`fix:`, `feat:`, `chore:`, ...) |
| **Branch** | A feature branch, never `main` (branch-protected) |

The gitleaks hook fires automatically on commit (pinned to v8.21.2 — later
versions are pathologically slow on ARM).

### 5.6 Branching, PRs, and the human gate

**Branch naming matters.** The auto-pr workflow only reacts to three prefixes:

```
fix/**    feat/**    feature/**
```

If the agent pushes a branch named `docs/...` or `anything/...`, CI still runs
but **no PR is opened** (and the agent can't open one — its token has no PR
write). The skill knows to use `fix/`/`feat/`/`feature/` always. If you ever see
a push with no PR, that's the first thing to check.

**The merge gate is human-only:**

1. Agent pushes branch → `auto-pr.yml` opens PR as `github-actions[bot]` (~30s)
2. You get a Telegram ping with the link (that's your GitHub Mobile tap target)
3. Required checks must pass before the merge button enables:
   - `gitleaks`
   - `Cross-Compile (linux/arm64)`
4. You merge. That's it — the deploy is automatic.

> **"This workflow requires approval" on a PR's checks?** That's the public-repo
> gate: runs triggered by the *PR-creation* event (actor = github-actions[bot])
> sit in `action_required` until a human clicks approve. Push-triggered runs
> (actor = you or the agent's push) run free. If a PR's first checks look stuck,
> a fresh push to the branch re-triggers them with the trusted actor and they
> complete normally. Merging is still always human-only.

### 5.7 Deploy, verify, roll back

**After you merge**

- CI builds the artifact (packaged into `dist/` — a top-level `pkg/` dir would
  collide with the source tree)
- The watchdog (`picoclaw-deploy.service` on the Pi, polling every 60s)
  downloads it, pre-flights, health-checks, and flips `current/`
- If health fails, it **auto-rolls-back** to the previous release (once), then
  gives up after repeated failures with a Telegram 🚨

**Verifying from chat**

```
you:  what's the status?
```

The agent reports: latest CI run, watchdog state, current release, and health
of both ports (`:18800` launcher, `:18790` gateway).

**Manual rollback (agent does this on request)**

```bash
ls -1 /opt/picoclaw/releases/
# pick the previous timestamp, flip the symlink, restart the launcher
ln -sfn /opt/picoclaw/releases/<PREV> /opt/picoclaw/current
systemctl --user restart picoclaw-launcher
```

There's also a known-good snapshot at `/opt/picoclaw/stable/` (never
overwritten) for the "just go back to what worked" case.

### 5.8 Failure handling: the agent's decision rules

When something breaks mid-cycle, the agent doesn't guess — it follows a policy:

| Situation | What the agent does |
|-----------|---------------------|
| **Easy, unambiguous** (lint error, typo, gitleaks false positive, trivial flake) | Auto-fixes, re-runs the exact failing check, re-pushes to the **same branch**, pings "Fixed → CI re-running" |
| **Unclear cause / multiple plausible fixes / touches behavior** | **Stops and reports** — what failed, what logs showed, the 2–3 candidate fixes it's weighing, what it'd pick. Waits for your call. |
| **Needs your decision** (scope change, dependency bump, config change) | **Stops and reports.** Never auto-applies changes outside the immediate failure. |

Guardrails: max 2 auto-fix attempts per cycle; auto-fix never touches `main`;
a fix that would grow beyond ~10 lines / 1 file = scope creep → stop and report;
real secrets are never "fixed" — the push is blocked server-side and rotation
policy applies.

### 5.9 Chat commands cheat-sheet

| You say | What happens |
|---------|--------------|
| `/use dev-cycle <message>` | Run a full change through the loop (pinged at every step) |
| "ship the fix for X" / "make X do Y" | Natural-language dev-cycle trigger |
| "what's the status?" | One-shot report: CI, watchdog, release, health (no step pings) |
| "roll back to the previous release" | Agent flips symlink + restarts launcher + verifies |
| "why did CI fail?" | Agent pulls run logs and explains, then applies failure policy |
| `/use clear` | Cancel an armed skill |

---

## 6. Troubleshooting common situations

### 6.1 Standard problems

**A deploy just failed / rolled back**

Read the watchdog log; the emoji sequence tells the story. Then check
`~/.picoclaw/logs/launcher.log` for why the new binary crashed. Usually it's
the watchdog doing its job — push a fix, merge, re-deploy. In chat, just say
"why did the deploy fail?" — the agent reads the logs and reports.

**The watchdog isn't deploying a build**

- Check `.last-deployed-run` — should match the CI run you expect.
- **Merged a docs-only / script-only change?** That's a **no-op build by
  design**: the workflow runs (so the required `Cross-Compile` check passes)
  but skips the build/upload steps, and the watchdog records the run in
  `.skipped-runs` with a ℹ️ Telegram notice. No new release is expected. This
  is the *correct* fast path, not a failure.
- `.failed-runs` — a run with count ≥ 2 is permanently skipped (retry-once
  policy by design). Push a new commit to re-trigger.
- Artifact expired (retention is 1 day)? The Pi was probably offline. Next
  push builds and deploys normally.

**CI shows "action_required" / "requires approval"**

Bot-authored PR runs in a public repo sometimes gate behind maintainer
approval. A fresh push to the branch (which auto-pr does) re-triggers with
you as the actor and runs ungated. Stale red runs from the first trigger can
be ignored.

**The gateway is down but the launcher is up**

```bash
systemctl --user restart picoclaw-launcher   # restarts launcher AND respawns gateway
tail -n 50 ~/.picoclaw/logs/gateway.log
```

If a restart happens during a deploy window the watchdog may see a transient
health-check failure and roll back — expected behavior.

**`journalctl --user` says "No journal files"**

Systemd quirk on newer Debian (user journal sockets never created). Use
`journalctl _SYSTEMD_USER_UNIT=<unit>` for service stdout, and
`tail -f ~/.picoclaw/logs/*.log` for launcher/gateway. Cosmetic — nothing in
the pipeline depends on `journalctl --user`.

### 6.2 Gotchas worth knowing (learned the hard way)

- **Branch prefix = PR or no PR.** `fix/` `feat/` `feature/` only. Cost a cycle
  when a `docs/` branch produced no auto-PR.
- **`edit_file` strips exec/read bits on scripts.** After editing any script,
  the agent checks the mode and restores it (`chmod u+rwx,go+rx` for exec
  scripts, `u+rw,go+r` otherwise). A live deploy script at 0600 broke a
  `systemctl restart` with `status=203/EXEC` once.
- **Exec guard quirks.** `git push` is allow-listed (the agent can push). Still
  blocked: `sudo`, `pkill`, `ssh host@`, heredocs, `${...}`/`$(...)` command
  substitution, numeric `chmod`. The agent uses plain sequential commands and
  symbolic chmod.
- **Watchdog restart is safe** and doesn't touch the launcher/gateway:
  `systemctl --user restart picoclaw-deploy`.

---

## 7. Security model

Because the repo is **public** (the price of enforced branch protection on
the free plan), the model is layered and explicitly least-privilege:

| Layer | What it does |
|-------|--------------|
| Branch protection on `main` | PR required + required checks + up-to-date + include admins → **no one** pushes straight to main, including you and the Pi's deploy key |
| Human merge gate | Only you can merge. The pipeline deploys only `main`. |
| gitleaks pre-commit hook (Pi) | Fast local feedback; convenience, defeatable by design |
| GitHub push protection | Server-side *prevention* — blocks known secret types on every push, any client |
| gitleaks CI (required check) | Server-side *detection* — runs where the Pi can't fake it; a merge gate |
| Secret hygiene | **No real secrets in the working tree, ever.** Tokens live in `config.json`/`.security.yml` on the Pi, injected at runtime. A public-repo leak is then a nothingburger + tripwire ("rotate now"). |
| Fine-grained PAT | Single repo; `Actions: read` + `Contents: read/write` only; **no Pull requests** (can't create/approve/merge PRs); no admin; short expiry |
| Deploy key | Single repo, write access only to that repo |
| Exec guard (assistant) | The AI's shell blocks `sudo`, `pkill`, `ssh host@`, heredocs, `${...}`/`$(...)`, numeric `chmod`; only `git push` is allow-listed — it can push branches but not run privileged/obfuscated commands |
| Notifications | Every deploy/rollback/emergency hits Telegram — an unauthorized change is immediately visible |

**The trust boundary in one sentence:** the AI assistant can *write code and
push branches*, but it cannot merge, cannot touch `main`, cannot run
privileged commands, and cannot hide a deploy. Everything that actually runs
on the Pi passes through your merge + the watchdog's health checks.

**Prompt-injection note:** the highest-stakes path is text → code → deployed
binary with API keys. Treat any fetched web content (docs, search results,
issues) as *data, not instructions*; never take code from anywhere except the
user in chat; review diffs before merging.

---

## 8. Optional hardening

**Failure drills (~15 min, recommended).** Practice while you're paying
attention — and do them **from chat** with the agent so you also learn its
failure-handling behavior:

1. *Failure-report drill* — ask the agent to ship a change with an intentional
   syntax error. CI fails, no deploy happens, and you practice the "fix and
   re-push" flow. (Branch protection already prevents merging a CI-failing
   commit, so this validates the pre-merge path.)
2. *Rollback drill* — merge something that passes pre-flight but fails health
   (e.g., a launcher that exits immediately). Watch the watchdog roll back,
   retry once, then permanently skip with 🚨. Then revert.
3. *Stable-fallback drill (optional)* — corrupt the previous release too, and
   verify it falls back to `stable/`.
4. *Manual rollback drill (optional)* — symlink-swap to an older release,
   restart, verify.

**Stronger merge gates (only if you ever relax branch protection).** Tier 1:
a TOTP wrapper around `git push` to `main` (`oathtool` + a shared secret).
Tier 2: a FIDO2 hardware-key SSH key (`ed25519-sk -O verify-required`) that
requires a physical touch for every push. Tier 3 (the default here): the
agent simply can't touch `main` — you merge PRs from your phone. Tiers 1/2
are friction you only need if you remove branch protection.

**Workflow-side Telegram PR notifications (optional).** The agent pings you
with auto-pr links; if you also want GitHub to notify you about *manual*
pushes, add a notify step inside `auto-pr.yml` using repo secrets
(`TG_BOT_TOKEN`, `TG_CHAT_ID`). One-time GitHub settings step; not needed for
the agent-driven flow.

---

## 9. Reference

### File inventory (repo)

| File | Purpose |
|------|---------|
| `.github/workflows/build-arm64.yml` | Cross-compile both binaries, package `dist/`, upload artifact (main only) |
| `.github/workflows/gitleaks.yml` | Server-side secret scan, all branches |
| `.github/workflows/auto-pr.yml` | Auto-open PR as `github-actions[bot]` on `fix/**`/`feat/**`/`feature/**` push |
| `scripts/setup-picoclaw.sh` | One-time Pi bootstrap |
| `deploy-picoclaw.sh` | Watchdog script (lives in repo; copied to `/opt/picoclaw/`) |
| `docs/pipeline-readme.MD` | This guide — complete setup + self-authoring reference |
| `skills/dev-cycle/SKILL.md` | Agent dev-cycle knowledge — the engine of self-authoring |

### File inventory (Pi)

| Path | Purpose |
|------|---------|
| `/opt/picoclaw/current` | Symlink → active release |
| `/opt/picoclaw/releases/<ts>/` | One dir per deploy (both binaries) |
| `/opt/picoclaw/stable/` | Known-good fallback (never overwritten) |
| `/opt/picoclaw/deploy-picoclaw.sh` | Watchdog script |
| `/opt/picoclaw/.last-deployed-run` | Last deployed CI run ID |
| `/opt/picoclaw/.skipped-runs` | CI runs skipped as no-op (no build-relevant change) |
| `/opt/picoclaw/.failed-runs` | Retry-once skip registry |
| `~/.config/systemd/user/picoclaw-launcher.service` | Launcher service |
| `~/.config/systemd/user/picoclaw-deploy.service` | Watchdog service |
| `~/.config/systemd/user/picoclaw-logrotate.{service,timer}` | Log rotation |
| `~/.config/logrotate/picoclaw.conf` | Logrotate rules |
| `~/.picoclaw/logs/{gateway,launcher}.log` | JSONL logs |
| `~/.picoclaw/.security.yml` | Telegram bot token |
| `~/.ssh/picoclaw-deploy` | Repo-scoped deploy key |
| `~/.ssh/config` (`Host github-picoclaw`) | SSH routing for the deploy key |

### Command cheat sheet

```bash
# Status
systemctl --user is-active picoclaw-launcher picoclaw-deploy picoclaw-logrotate.timer
readlink /opt/picoclaw/current
cat /opt/picoclaw/.last-deployed-run

# Logs
journalctl _SYSTEMD_USER_UNIT=picoclaw-deploy.service -f
tail -f ~/.picoclaw/logs/gateway.log
tail -f ~/.picoclaw/logs/launcher.log

# Deploy ops
systemctl --user restart picoclaw-launcher     # respawns gateway too
systemctl --user restart picoclaw-deploy       # watchdog only, safe
systemctl --user kill -s USR1 picoclaw-deploy  # force an immediate poll

# Rollback (no sudo)
ln -sfn /opt/picoclaw/releases/<TS> /opt/picoclaw/current
systemctl --user restart picoclaw-launcher

# Log rotation
systemctl --user list-timers picoclaw-logrotate
cat ~/.local/state/logrotate.status
```

### Chat cheat sheet

| You say | What happens |
|---------|--------------|
| `/use dev-cycle <message>` | Run a full change through the loop (pinged at every step) |
| "what's the status?" | One-shot report: CI, watchdog, release, health |
| "roll back to the previous release" | Agent flips symlink + restarts launcher + verifies |
| "why did CI fail?" | Agent pulls run logs and explains, then applies failure policy |

---

*Complete setup + self-authoring guide (v3). Written for a fresh setup with
the chat-driven dev loop as the primary path. Maintained by PicoClaw 🦞 —
2026-08-03. Replaces v2; the v1 day-to-day guide remains for status checks.
Versioned in the repo at `docs/pipeline-readme.MD` (mirrors the workspace
`PIPELINE_USER_GUIDE_v3.md`).*
