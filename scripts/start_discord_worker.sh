#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="${1:-${ROOT_DIR}/scripts/discord_worker.env}"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "env file not found: ${ENV_FILE}" >&2
  echo "copy scripts/discord_worker.env.example to scripts/discord_worker.env and edit values." >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

# Resolve helper scripts from this repository even when CHATOPS_WORKDIR points to another repo.
if [[ -z "${CHATOPS_RUN_SCRIPT:-}" ]]; then
  export CHATOPS_RUN_SCRIPT="${ROOT_DIR}/scripts/chatops_run_task.sh"
fi
if [[ -z "${CHATOPS_MERGE_SCRIPT:-}" ]]; then
  export CHATOPS_MERGE_SCRIPT="${ROOT_DIR}/scripts/chatops_merge_branch.sh"
fi
if [[ -z "${CHATOPS_DISCARD_SCRIPT:-}" ]]; then
  export CHATOPS_DISCARD_SCRIPT="${ROOT_DIR}/scripts/chatops_discard_branch.sh"
fi

cd "${ROOT_DIR}"
exec go run ./cmd/discord-worker
