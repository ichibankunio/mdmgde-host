# mdmgde-host

Discord ChatOps + preview hosting repository.

## Purpose

This repository is the **host/orchestrator** side.
- Run Discord bot and worker process.
- Execute Codex-driven tasks against a separate private game repository.
- Publish preview artifacts to this repository's `docs/previews/...` and serve via GitHub Pages.

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
CHATOPS_PREVIEW_URL_TEMPLATE="https://ichibankunio.github.io/mdmgde-host/previews/{branch_slug}/"
CHATOPS_WAIT_PAGES_DEPLOY=true
CHATOPS_PAGES_TIMEOUT_SECONDS=240
```

`deploy_preview_to_pages.sh` copies `private/docs/*` first, and if `index.html` is missing there, it falls back to `private/web/index.html` (and `wasm_exec.js`) so each game can keep a game-specific launcher page.

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
docs/previews/<branch-slug>/
```

GitHub Pages should be enabled for this repository.
