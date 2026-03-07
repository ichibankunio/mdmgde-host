#!/usr/bin/env bash
set -euo pipefail

BRANCH="${1:-}"
JOB_ID="${2:-discard}"

if [[ -z "${BRANCH}" ]]; then
  echo '{"status":"error","message":"usage: chatops_discard_branch.sh <branch> [job-id]"}'
  exit 1
fi
if [[ "${BRANCH}" != task/* ]]; then
  echo '{"status":"error","message":"branch must start with task/"}'
  exit 1
fi

for cmd in git jq; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "{\"status\":\"error\",\"message\":\"${cmd} command is required\"}"
    exit 1
  fi
done

BASE_BRANCH="${CHATOPS_BASE_BRANCH:-main}"
STATE_DIR="${CHATOPS_STATE_DIR:-$(pwd)/.codex-worker/chatops}"
LOG_DIR="${STATE_DIR}/logs"
DELETE_REMOTE="${CHATOPS_DISCARD_DELETE_REMOTE:-true}"
mkdir -p "${LOG_DIR}"
LOG_FILE="${LOG_DIR}/${JOB_ID}.log"

json_fail() {
  local msg="$1"
  jq -c -n \
    --arg status "error" \
    --arg message "${msg}" \
    --arg log_file "${LOG_FILE}" \
    '{status:$status,message:$message,log_file:$log_file}'
}

if [[ -n "$(git status --porcelain)" ]]; then
  json_fail "worktree is dirty"
  exit 1
fi

if ! git fetch origin "${BASE_BRANCH}" >>"${LOG_FILE}" 2>&1; then
  json_fail "failed to fetch base branch"
  exit 1
fi
if ! git checkout "${BASE_BRANCH}" >>"${LOG_FILE}" 2>&1; then
  json_fail "failed to checkout base branch"
  exit 1
fi
if ! git pull --ff-only origin "${BASE_BRANCH}" >>"${LOG_FILE}" 2>&1; then
  json_fail "failed to update base branch"
  exit 1
fi

if git show-ref --verify --quiet "refs/heads/${BRANCH}"; then
  git branch -D "${BRANCH}" >>"${LOG_FILE}" 2>&1 || true
fi

if [[ "${DELETE_REMOTE}" == "true" ]]; then
  if git ls-remote --exit-code --heads origin "${BRANCH}" >/dev/null 2>&1; then
    git push origin --delete "${BRANCH}" >>"${LOG_FILE}" 2>&1 || true
  fi
fi

jq -c -n \
  --arg status "ok" \
  --arg branch "${BRANCH}" \
  --arg message "discarded ${BRANCH} and returned to ${BASE_BRANCH}" \
  --arg log_file "${LOG_FILE}" \
  '{status:$status,branch:$branch,message:$message,log_file:$log_file}'
