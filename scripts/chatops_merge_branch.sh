#!/usr/bin/env bash
set -euo pipefail

BRANCH="${1:-}"
JOB_ID="${2:-merge}"

if [[ -z "${BRANCH}" ]]; then
  echo '{"status":"error","message":"usage: chatops_merge_branch.sh <branch> [job-id]"}'
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
if ! git fetch origin "${BRANCH}" >>"${LOG_FILE}" 2>&1; then
  json_fail "failed to fetch target branch"
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
if ! git merge --no-ff "origin/${BRANCH}" -m "merge(chatops): ${BRANCH}" >>"${LOG_FILE}" 2>&1; then
  json_fail "merge failed"
  exit 1
fi
if ! git push origin "${BASE_BRANCH}" >>"${LOG_FILE}" 2>&1; then
  json_fail "push failed"
  exit 1
fi

if [[ "${CHATOPS_DELETE_BRANCH:-false}" == "true" ]]; then
  git push origin --delete "${BRANCH}" >>"${LOG_FILE}" 2>&1 || true
fi

commit="$(git rev-parse --short HEAD)"
jq -c -n \
  --arg status "ok" \
  --arg branch "${BRANCH}" \
  --arg message "merged to ${BASE_BRANCH} at ${commit}" \
  --arg log_file "${LOG_FILE}" \
  '{status:$status,branch:$branch,message:$message,log_file:$log_file}'
