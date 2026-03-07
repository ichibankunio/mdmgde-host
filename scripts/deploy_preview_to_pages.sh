#!/usr/bin/env bash
set -euo pipefail

BRANCH="${CHATOPS_BRANCH:-}"
if [[ -z "${BRANCH}" ]]; then
  echo "CHATOPS_BRANCH is required" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEFAULT_PUBLIC_REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
PRIVATE_REPO_DIR="$(pwd)"
PRIVATE_DOCS_DIR="${CHATOPS_PRIVATE_DOCS_DIR:-${PRIVATE_REPO_DIR}/docs}"
PUBLIC_REPO_DIR="${CHATOPS_PUBLIC_REPO_DIR:-${DEFAULT_PUBLIC_REPO_DIR}}"
BASE_BRANCH="${CHATOPS_PUBLIC_BASE_BRANCH:-main}"
PREVIEW_ROOT="${CHATOPS_PREVIEW_ROOT_DIR:-docs/previews}"
WAIT_PAGES="${CHATOPS_WAIT_PAGES_DEPLOY:-true}"
PAGES_TIMEOUT_SECONDS="${CHATOPS_PAGES_TIMEOUT_SECONDS:-240}"
PAGES_POLL_INTERVAL_SECONDS="${CHATOPS_PAGES_POLL_INTERVAL_SECONDS:-5}"

if [[ ! -d "${PRIVATE_DOCS_DIR}" ]]; then
  echo "private docs dir not found: ${PRIVATE_DOCS_DIR}" >&2
  exit 1
fi
if [[ ! -d "${PUBLIC_REPO_DIR}/.git" ]]; then
  echo "public repo dir is not a git repo: ${PUBLIC_REPO_DIR}" >&2
  exit 1
fi

slug="$(printf '%s' "${BRANCH}" | tr '[:upper:]' '[:lower:]' | sed -E 's#[^a-z0-9]+#-#g; s#(^-+|-+$)##g')"
if [[ -z "${slug}" ]]; then
  echo "failed to build branch slug" >&2
  exit 1
fi

TARGET_DIR="${PUBLIC_REPO_DIR}/${PREVIEW_ROOT}/${slug}"
# Use a temporary clean worktree so deployment works even if PUBLIC_REPO_DIR is dirty.
WORKTREE_DIR="$(mktemp -d)"
cleanup() {
  git -C "${PUBLIC_REPO_DIR}" worktree remove --force "${WORKTREE_DIR}" >/dev/null 2>&1 || true
  rm -rf "${WORKTREE_DIR}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

git -C "${PUBLIC_REPO_DIR}" fetch origin "${BASE_BRANCH}" >/dev/null 2>&1
git -C "${PUBLIC_REPO_DIR}" worktree add --detach "${WORKTREE_DIR}" "origin/${BASE_BRANCH}" >/dev/null 2>&1

cd "${WORKTREE_DIR}"
TARGET_DIR="${WORKTREE_DIR}/${PREVIEW_ROOT}/${slug}"
mkdir -p "${TARGET_DIR}"
# Replace preview directory contents for this branch.
find "${TARGET_DIR}" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
shopt -s nullglob
for item in "${PRIVATE_DOCS_DIR}"/*; do
  cp -R "${item}" "${TARGET_DIR}/"
done
shopt -u nullglob

git add -f "${PREVIEW_ROOT}/${slug}"
target_sha=""
if git diff --cached --quiet; then
  target_sha="$(git rev-parse "origin/${BASE_BRANCH}")"
else
  git commit -m "deploy(preview): ${BRANCH}" >/dev/null 2>&1
  target_sha="$(git rev-parse HEAD)"
  git push origin "HEAD:${BASE_BRANCH}" >/dev/null 2>&1
fi

wait_for_pages_deploy() {
  local repo_full run_info run_status run_conclusion run_id elapsed
  if ! command -v gh >/dev/null 2>&1; then
    return 0
  fi
  repo_full="$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null || true)"
  if [[ -z "${repo_full}" ]]; then
    return 0
  fi
  elapsed=0
  while (( elapsed < PAGES_TIMEOUT_SECONDS )); do
    run_info="$(
      gh run list \
        --repo "${repo_full}" \
        --workflow pages.yml \
        --branch "${BASE_BRANCH}" \
        --event push \
        --limit 20 \
        --json databaseId,headSha,status,conclusion \
        --jq "[.[] | select(.headSha == \"${target_sha}\")][0] | @json" \
        2>/dev/null || true
    )"
    if [[ -n "${run_info}" && "${run_info}" != "null" ]]; then
      run_status="$(printf '%s' "${run_info}" | jq -r '.status')"
      run_conclusion="$(printf '%s' "${run_info}" | jq -r '.conclusion // ""')"
      run_id="$(printf '%s' "${run_info}" | jq -r '.databaseId')"
      if [[ "${run_status}" == "completed" ]]; then
        if [[ "${run_conclusion}" == "success" ]]; then
          return 0
        fi
        echo "pages workflow failed for run ${run_id} (conclusion=${run_conclusion})" >&2
        return 1
      fi
    fi
    sleep "${PAGES_POLL_INTERVAL_SECONDS}"
    elapsed=$((elapsed + PAGES_POLL_INTERVAL_SECONDS))
  done
  echo "timeout waiting for Pages deploy (sha=${target_sha})" >&2
  return 1
}

if [[ "${WAIT_PAGES}" == "true" ]]; then
  if ! wait_for_pages_deploy; then
    exit 1
  fi
fi

build_default_url() {
  local owner_repo remote owner repo
  if command -v gh >/dev/null 2>&1; then
    owner_repo="$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null || true)"
  fi
  if [[ -z "${owner_repo}" ]]; then
    remote="$(git config --get remote.origin.url || true)"
    owner_repo="$(printf '%s' "${remote}" | sed -E 's#^git@github.com:##; s#^https://github.com/##; s#\.git$##')"
  fi
  owner="${owner_repo%%/*}"
  repo="${owner_repo##*/}"
  printf 'https://%s.github.io/%s/previews/%s/' "${owner}" "${repo}" "${slug}"
}

if [[ -n "${CHATOPS_PREVIEW_URL_TEMPLATE:-}" ]]; then
  url="${CHATOPS_PREVIEW_URL_TEMPLATE}"
  url="${url//\{branch\}/${BRANCH}}"
  url="${url//\{branch_slug\}/${slug}}"
  printf '%s\n' "${url}"
else
  build_default_url
fi
