# mdmgde-host

Discord ChatOps + preview hosting repository.

## Purpose

This repository is the **host/orchestrator** side.
- Run Discord bot and worker process.
- Execute Codex-driven tasks against a separate private game repository.
- Publish preview artifacts to this repository's `docs/latest` and serve via GitHub Pages.

## Core Components

- Bot runtime: `cmd/discord-worker/`
- ChatOps scripts: `scripts/chatops_*.sh`
- Preview deploy helper: `scripts/deploy_preview_to_pages.sh`
- Worker launcher: `scripts/start_discord_worker.sh`

## Prerequisites

```bash
gh auth login
codex login
```

## Setup

### 1. GitHub repository settings

- `Settings > Pages`
  - `Build and deployment`: `GitHub Actions`
- `Settings > Actions > General`
  - `Workflow permissions`: `Read and write permissions`

### 2. Discord bot settings

- Bot OAuth scopes: `bot`, `applications.commands`
- Bot permissions (minimum):
  - `View Channels`
  - `Send Messages`
  - `Read Message History`
  - `Use Slash Commands`
- For thread-based workflow:
  - `Create Public Threads`
  - `Send Messages in Threads`
- Developer Portal > Bot > Privileged Gateway Intents:
  - Enable `Message Content Intent`

### 3. Create env file

## Configure

```bash
cp scripts/discord_worker.env.example scripts/discord_worker.env
```

Set at least:
- `DISCORD_BOT_TOKEN`
- `DISCORD_APP_ID`
- `DISCORD_GUILD_ID`
- `CHATOPS_PROJECTS` (`repo -> private workdir` mapping)
- `CHATOPS_PREVIEW_CMD` (defaults provided)

`CHATOPS_PROJECTS` examples:

```bash
# JSON
CHATOPS_PROJECTS='{"owner/game-a":"/repos/game-a","owner/game-b":"/repos/game-b"}'

# CSV
CHATOPS_PROJECTS="owner/game-a=/repos/game-a,owner/game-b=/repos/game-b"
```

Recommended preview settings:

```bash
CHATOPS_PREVIEW_CMD="/Users/ichibankunio/go/src/github.com/ichibankunio/mdmgde-host/scripts/deploy_preview_to_pages.sh"
CHATOPS_PUBLIC_REPO_DIR="/Users/ichibankunio/go/src/github.com/ichibankunio/mdmgde-host"
CHATOPS_PRIVATE_WEB_DIR="/abs/path/to/private-repo/web"
CHATOPS_WASM_EXEC_JS="/abs/path/to/go/lib/wasm/wasm_exec.js" # optional
CHATOPS_PREVIEW_SINGLE_SLOT=true
CHATOPS_PREVIEW_TARGET_DIR="docs/latest"
CHATOPS_PREVIEW_URL_TEMPLATE="https://ichibankunio.github.io/mdmgde-host/"
CHATOPS_WAIT_PAGES_DEPLOY=true
CHATOPS_PAGES_TIMEOUT_SECONDS=600
CHATOPS_PAGES_WAIT_STRICT=false
CHATOPS_DELETE_BRANCH=true
CHATOPS_DISCARD_DELETE_REMOTE=true
```

Optional email notification settings:

```bash
CHATOPS_NOTIFY_EMAIL_TO="dev1@example.com,dev2@example.com"
CHATOPS_NOTIFY_EMAIL_FROM="chatops@example.com"
CHATOPS_NOTIFY_SMTP_HOST="smtp.example.com"
CHATOPS_NOTIFY_SMTP_PORT=587
CHATOPS_NOTIFY_SMTP_USER="smtp-user"      # optional
CHATOPS_NOTIFY_SMTP_PASS="smtp-password"  # required only when SMTP_USER is set
```

Email is sent when:
- preview deploy is completed (`/run`, `/improve` successful with preview URL)
- merge is completed (`/merge`)
- Codex requests additional user input (`need_input`)

`deploy_preview_to_pages.sh` copies `private/docs/*` first, and if `index.html` is missing there, it falls back to `private/web/index.html` so each game can keep a game-specific launcher page.
For `wasm_exec.js`, it prioritizes `CHATOPS_WASM_EXEC_JS`, then the active Go toolchain (`go env GOROOT`), and finally `private/web/wasm_exec.js`. This keeps `game.wasm` and `wasm_exec.js` in sync across Go version upgrades.
If `private/docs` does not contain `favicon.ico`, it also falls back to `private/web/favicon.ico`.
`CHATOPS_PREVIEW_SINGLE_SLOT=true` keeps only one preview directory (default `docs/latest`) and replaces its contents on each deploy.
It also appends a cache-buster query to `game.wasm` and `wasm_exec.js` references in `index.html` on each deploy to avoid stale browser cache mismatches.
When `CHATOPS_WAIT_PAGES_DEPLOY=true`, the script waits for `pages.yml`. By default this wait is non-fatal (`CHATOPS_PAGES_WAIT_STRICT=false`) and only logs a warning on timeout/failure.

Legacy single-project config (`CHATOPS_WORKDIR` + `CHATOPS_ALLOWED_REPOS`) is still supported.

### 4. Start worker

```bash
./scripts/start_discord_worker.sh
```

### 5. Smoke test

1. Run `/start` in Discord.
2. Run `/run` and complete one task.
3. Confirm preview URL is returned.
4. Confirm Pages workflow (`Deploy Hosted Previews to GitHub Pages`) succeeds.

## Main Commands

- `/start`
- `/run`
- `/improve`
- `/preview`
- `/merge`
- `/discard`
- `/status`
- `/logs`

## Pages Hosting

This repo expects preview files under:

```text
docs/latest
```

GitHub Pages should be enabled for this repository.
