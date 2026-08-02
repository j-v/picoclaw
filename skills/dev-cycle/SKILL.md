---
name: dev-cycle
description: "Run the picoclaw dev cycle: check in code, run pre-push checks, push to a feature branch, open a PR, monitor CI builds, check watchdog/deploy status, roll back a bad release, or ship a fix. Use when the user wants to change, commit, push, build, deploy, or revert picoclaw itself."
---

# Dev Cycle Skill

Domain knowledge for the PicoClaw development pipeline. The mechanical build→deploy
loop is handled by the **watchdog** (`deploy-picoclaw.sh` on the Pi). This skill gives
the agent the knowledge to gate, coordinate, monitor, and report on the whole lifecycle.

Reference architecture: `PIPELINE_PLAN.md` (workspace).

## Trigger paths

| Trigger | How |
|---------|-----|
| **Slash command (explicit)** | `/use dev-cycle <message>` — forces the skill for one request; `/use dev-cycle` arms it for the next message; `/use clear` cancels |
| **Natural language (implicit)** | Match the user's message against this skill's `description` (the NL key) |

## The full dev cycle loop — ping at every step

When running a change through the pipeline, announce each step so the user always
knows where the cycle is. **No silent phases.**

```
1. ✏️  Make code changes (edit files via agent tools)
     → ping: "Step 1/7 — editing files"
2. 🔍 Pre-push checklist:
   ├── git status — only expected files changed?
   ├── gitleaks detect — no secrets leaked?
   ├── no debug code left behind (console.log, fmt.Println, etc.)
   ├── config changes reviewed (no API keys in source)?
   └── commit message drafted and meaningful?
     → ping: "Step 2/7 — pre-push checks: ✅ passed / ❌ blocked"
3. ✅ Git commit
     → ping: "Step 3/7 — committed <sha>"
4. 🚀 Git push to feature branch + open PR
     → ping: "Step 4/7 — pushed <branch>, PR #<n>: <url>"
5. 👀 Build monitoring:
   ├── Watchdog tight-polls automatically
   └── Agent can report: "Build #1429 in progress..."
     → ping: "Step 5/7 — CI: ✅ passed / ❌ failed"
6. ✅ Automated deploy (watchdog handles it — after YOU merge to main)
     → ping: "Step 6/7 — deploy started / rolled back"
7. 🏥 Post-deploy verification:
   ├── Health check passed (HTTP :18800 + TCP :18790)?
   └── Agent can confirm: "Deploy succeeded, running release 20260730-... "
     → ping: "Step 7/7 — ✅ done, running <release>"
```

## Pre-push checklist (run BEFORE any commit)

| Check | What it verifies |
|-------|-----------------|
| `git status` | Only files we intend to commit are modified |
| **No secrets** | Gitleaks pre-commit hook handles this automatically; agent also checks no API keys, tokens, or `.security.yml` references are in the diff |
| **No debug code** | No `console.log()`, `println()`, `fmt.Print*()`, `debug.*`, or `TODO` comments intended for development only |
| **Config sanity** | Any changes to `config.json`, `.env`, or YAML files are intentional and don't hardcode credentials |
| **Commit message** | Descriptive, conventional commit format (`feat:`, `fix:`, `chore:`, etc.) |
| **Branch** | Are we on `main`? Should be on a feature branch (`fix/...`, `feat/...`) |

## Push + PR workflow (Tier 3 — human-gated merge)

The agent pushes to a **feature branch only**. `main` is branch-protected: PR required,
human merge. Never attempt a direct push to `main`.

```bash
git checkout -b fix/<desc>
git add -A && git commit -m "fix: <desc>"
git push origin fix/<desc>
gh pr create --fill            # then ping the user with the PR link
# USER merges via GitHub Mobile / web UI / CLI on a separate device
```

## Pre-deploy checklist (before any deploy action or force-poll)

| Check | What it verifies |
|-------|-----------------|
| **CI status** | Latest build on `main` is green (`conclusion=success`) |
| **Run ID** | Confirms the run ID we're deploying differs from `last-deployed-run` |
| **Watchdog health** | `systemctl --user is-active picoclaw-deploy.service` = `active`. **If inactive:** `systemctl --user start picoclaw-deploy.service`, wait 5s, re-verify. |
| **Launcher health** | `curl -sf http://localhost:18800` responds |
| **Gateway health** | `nc -z localhost 18790` accepts connections |
| **Artifact ready** | The run's artifact is available (check before deploy, not during) |

## Status report (no step pings — this is a query, not a cycle)

When asked "what's the status?", report:

1. Latest CI run: `gh run list --repo j-v/picoclaw --workflow build-arm64.yml --branch main --limit 1` → "Build #N passed (abc1234)"
2. Watchdog: `systemctl --user is-active picoclaw-deploy.service` + `cat /opt/picoclaw/.last-deployed-run`
3. Current release: `readlink /opt/picoclaw/current`
4. Health: `curl -sf http://localhost:18800` + `nc -z localhost 18790`

## Rollback (manual)

```bash
ls -1 /opt/picoclaw/releases/
PREV=<timestamp>
ln -sfn /opt/picoclaw/releases/$PREV /opt/picoclaw/current
systemctl --user restart picoclaw-launcher
```

Or fall back to the known-good snapshot:

```bash
ln -sfn /opt/picoclaw/stable /opt/picoclaw/current
systemctl --user restart picoclaw-launcher
```

## Failure handling (auto-fix easy, stop on ambiguity)

| Failure | Response |
|---------|----------|
| **Easy, unambiguous fix** (lint error, missing import, typo, gitleaks false positive, trivial test flake) | **Auto-fix and re-push.** Make the minimal change, re-run the failed check, commit to the same feature branch, push again, ping: "Fixed <x> → re-pushed <branch>, CI re-running" |
| **Unclear cause / multiple plausible fixes / touches behavior** (test logic disagreement, build error that could mean several things, deploy failure with ambiguous logs) | **Stop and report.** Leave the branch as-is, ping with: what failed, what I saw in the logs, the 2–3 candidate fixes I'm weighing, and what I'd pick — wait for the user's call before touching anything else |
| **Anything that needs a decision the user would want to make** (changing scope, bumping a dependency, altering config) | **Stop and report** — same as above. Never auto-apply changes outside the immediate failure being fixed |

**Guardrails on auto-fix:**
- Max **2 auto-fix attempts** per cycle; after that, stop and report (prevents fix-loops)
- Auto-fix only commits to the **same feature branch** — never touches `main`, never bypasses the PR/merge gate
- Auto-fix must re-run the **exact failing check** before re-pushing (CI green on the new commit, not assumed)
- If a fix would touch more than ~10 lines or more than one file beyond the original change, **stop and report** — that's scope creep, not a fix
- Gitleaks findings: only auto-fix **false positives** (add `gitleaks:allow` inline or allowlist entry). A real secret means stop — the push is already blocked server-side, and rotation policy applies

## Relationship to the watchdog

| Component | Role |
|-----------|------|
| **Watchdog** (`deploy-picoclaw.sh`) | Fully automated poll-and-deploy loop. Runs 24/7 as a user systemd service. |
| **Dev cycle skill** | Agent knowledge of the full pipeline: pre-push checks, commit workflow, build monitoring, pre-deploy checks, status reports, rollback coordination. |

## Key commands cheat-sheet

```bash
# Watchdog control
systemctl --user status picoclaw-deploy.service
systemctl --user kill -s USR1 picoclaw-deploy.service   # force immediate poll
journalctl --user -u picoclaw-deploy.service -f         # follow deploy logs
journalctl --user -u picoclaw-launcher.service -n 50 --no-pager

# CI status
gh run list --repo j-v/picoclaw --workflow build-arm64.yml --branch main --limit 3
gh run view <run-id> --repo j-v/picoclaw

# Deployed state
readlink /opt/picoclaw/current
cat /opt/picoclaw/.last-deployed-run
ls -1 /opt/picoclaw/releases/
```
