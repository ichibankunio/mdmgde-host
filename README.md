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

Legacy single-project config (`CHATOPS_WORKDIR` + `CHATOPS_ALLOWED_REPOS`) is still supported.

## Start Worker

```bash
./scripts/start_discord_worker.sh
```

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
