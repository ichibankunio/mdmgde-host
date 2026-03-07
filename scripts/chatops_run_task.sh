#!/usr/bin/env bash
set -euo pipefail

REPO="${1:-}"
TASK="${2:-}"
JOB_ID="${3:-}"

if [[ -z "${REPO}" || -z "${TASK}" || -z "${JOB_ID}" ]]; then
  echo '{"status":"error","message":"usage: chatops_run_task.sh <repo> <task> <job-id>"}'
  exit 1
fi

for cmd in git gh codex jq; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "{\"status\":\"error\",\"message\":\"${cmd} command is required\"}"
    exit 1
  fi
done

BASE_BRANCH="${CHATOPS_BASE_BRANCH:-main}"
TARGET_BRANCH="${CHATOPS_TARGET_BRANCH:-}"
STATE_DIR="${CHATOPS_STATE_DIR:-$(pwd)/.codex-worker/chatops}"
LOG_DIR="${STATE_DIR}/logs"
mkdir -p "${LOG_DIR}"
LOG_FILE="${LOG_DIR}/${JOB_ID}.log"

log() {
  echo "[$(date +%Y-%m-%dT%H:%M:%S%z)] [run_task:${JOB_ID}] $*" | tee -a "${LOG_FILE}" >&2
}

emit_progress() {
  echo "PROGRESS:$1"
}

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

actual_repo="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
if [[ "${actual_repo}" != "${REPO}" ]]; then
  json_fail "repo mismatch expected=${REPO} actual=${actual_repo}"
  exit 1
fi

if [[ -n "${TARGET_BRANCH}" ]]; then
  BRANCH="${TARGET_BRANCH}"
else
  BRANCH="task/$(date +%Y%m%d-%H%M%S)-${JOB_ID}"
fi
TMP_DIR="$(mktemp -d)"
PROMPT_FILE="${TMP_DIR}/prompt.txt"
RESPONSE_FILE="${TMP_DIR}/response.txt"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

emit_progress "sync"
log "sync base branch ${BASE_BRANCH}"
git fetch origin "${BASE_BRANCH}" >>"${LOG_FILE}" 2>&1
git checkout "${BASE_BRANCH}" >>"${LOG_FILE}" 2>&1
git pull --ff-only origin "${BASE_BRANCH}" >>"${LOG_FILE}" 2>&1

if [[ -n "${TARGET_BRANCH}" ]]; then
  emit_progress "checkout"
  log "checkout existing branch ${BRANCH}"
  git fetch origin "${BRANCH}" >>"${LOG_FILE}" 2>&1
  git checkout -B "${BRANCH}" "origin/${BRANCH}" >>"${LOG_FILE}" 2>&1
else
  emit_progress "branch"
  log "create branch ${BRANCH}"
  git checkout -b "${BRANCH}" >>"${LOG_FILE}" 2>&1
fi

cat >"${PROMPT_FILE}" <<PROMPT
Repository: ${REPO}

Goal:
${TASK}

Constraints:
- Do not modify files outside this repository.
- Keep changes minimal and reviewable.
- Prefer updating only game source and docs unless necessary.
- Run required checks and summarize results.
- Do not commit or push directly.

Additional constraints:
${CHATOPS_CONSTRAINTS:-None}

Task:
${TASK}

If you need a user decision before implementation, include a line:
QUESTION_FOR_USER: <question>
PROMPT

emit_progress "codex"
log "run codex"
set +e
codex exec --full-auto -C "$(pwd)" -o "${RESPONSE_FILE}" - <"${PROMPT_FILE}" >>"${LOG_FILE}" 2>&1
codex_status=$?
set -e
if [[ "${codex_status}" -ne 0 ]]; then
  json_fail "codex exec failed (status=${codex_status})"
  exit 1
fi

question="$(sed -n 's/^QUESTION_FOR_USER:[[:space:]]*//p' "${RESPONSE_FILE}" | head -n 1)"
if [[ -n "${question}" ]]; then
  emit_progress "waiting_input"
  if [[ -z "${TARGET_BRANCH}" ]]; then
    git checkout "${BASE_BRANCH}" >>"${LOG_FILE}" 2>&1 || true
    git branch -D "${BRANCH}" >>"${LOG_FILE}" 2>&1 || true
  fi
  jq -c -n \
    --arg status "need_input" \
    --arg branch "${TARGET_BRANCH}" \
    --arg message "${question}" \
    --arg summary "$(head -n 8 "${RESPONSE_FILE}" | tr '\n' ' ' | sed -E 's/[[:space:]]+/ /g')" \
    --arg log_file "${LOG_FILE}" \
    '{status:$status,branch:$branch,message:$message,summary:$summary,log_file:$log_file}'
  exit 0
fi

if [[ -n "${CHATOPS_TEST_CMD:-}" ]]; then
  emit_progress "test"
  log "run test command"
  if ! bash -lc "${CHATOPS_TEST_CMD}" >>"${LOG_FILE}" 2>&1; then
    json_fail "test command failed"
    exit 1
  fi
fi

if [[ -n "${CHATOPS_BUILD_CMD:-make wasm}" ]]; then
  emit_progress "build"
  log "run build command"
  if ! bash -lc "${CHATOPS_BUILD_CMD:-make wasm}" >>"${LOG_FILE}" 2>&1; then
    json_fail "build command failed"
    exit 1
  fi
fi

if [[ -z "$(git status --porcelain)" ]]; then
  if [[ -z "${TARGET_BRANCH}" ]]; then
    git checkout "${BASE_BRANCH}" >>"${LOG_FILE}" 2>&1
    git branch -D "${BRANCH}" >>"${LOG_FILE}" 2>&1
  fi
  summary="$(head -n 8 "${RESPONSE_FILE}" | tr '\n' ' ' | sed -E 's/[[:space:]]+/ /g')"
  jq -c -n \
    --arg status "no_changes" \
    --arg branch "" \
    --arg preview_url "" \
    --arg summary "${summary}" \
    --arg log_file "${LOG_FILE}" \
    '{status:$status,branch:$branch,preview_url:$preview_url,summary:$summary,log_file:$log_file}'
  exit 0
fi

emit_progress "push"
log "commit and push"
git add -A >>"${LOG_FILE}" 2>&1
git commit -m "feat(chatops): ${TASK}" >>"${LOG_FILE}" 2>&1
git push -u origin "${BRANCH}" >>"${LOG_FILE}" 2>&1

preview_url=""
if [[ -n "${CHATOPS_PREVIEW_CMD:-}" ]]; then
  emit_progress "deploy"
  log "deploy preview"
  set +e
  preview_url="$(CHATOPS_BRANCH="${BRANCH}" CHATOPS_JOB_ID="${JOB_ID}" bash -lc "${CHATOPS_PREVIEW_CMD}" 2>>"${LOG_FILE}")"
  preview_status=$?
  set -e
  if [[ "${preview_status}" -ne 0 ]]; then
    json_fail "preview deploy failed"
    exit 1
  fi
  preview_url="$(printf '%s' "${preview_url}" | tail -n 1 | tr -d '\r')"
fi

summary="$(head -n 8 "${RESPONSE_FILE}" | tr '\n' ' ' | sed -E 's/[[:space:]]+/ /g')"

emit_progress "done"
jq -c -n \
  --arg status "ok" \
  --arg branch "${BRANCH}" \
  --arg preview_url "${preview_url}" \
  --arg summary "${summary}" \
  --arg log_file "${LOG_FILE}" \
  '{status:$status,branch:$branch,preview_url:$preview_url,summary:$summary,log_file:$log_file}'
