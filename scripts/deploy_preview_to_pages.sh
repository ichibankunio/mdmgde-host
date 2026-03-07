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
PRIVATE_WEB_DIR="${CHATOPS_PRIVATE_WEB_DIR:-${PRIVATE_REPO_DIR}/web}"
PUBLIC_REPO_DIR="${CHATOPS_PUBLIC_REPO_DIR:-${DEFAULT_PUBLIC_REPO_DIR}}"
BASE_BRANCH="${CHATOPS_PUBLIC_BASE_BRANCH:-main}"
PREVIEW_ROOT="${CHATOPS_PREVIEW_ROOT_DIR:-docs/previews}"
PREVIEW_SINGLE_SLOT="${CHATOPS_PREVIEW_SINGLE_SLOT:-true}"
PREVIEW_TARGET_DIR="${CHATOPS_PREVIEW_TARGET_DIR:-docs/latest}"
PREVIEW_CACHE_BUSTER="${CHATOPS_PREVIEW_CACHE_BUSTER:-}"
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

target_rel="${PREVIEW_ROOT}/${slug}"
url_branch_slug="${slug}"
if [[ "${PREVIEW_SINGLE_SLOT}" == "true" ]]; then
  target_rel="${PREVIEW_TARGET_DIR}"
  url_branch_slug="$(basename "${PREVIEW_TARGET_DIR}")"
fi
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
preview_root_abs="${WORKTREE_DIR}/${PREVIEW_ROOT}"
TARGET_DIR="${WORKTREE_DIR}/${target_rel}"
mkdir -p "${preview_root_abs}"
if [[ "${PREVIEW_SINGLE_SLOT}" == "true" ]]; then
  target_base="$(basename "${target_rel}")"
  find "${preview_root_abs}" -mindepth 1 -maxdepth 1 ! -name "${target_base}" -exec rm -rf {} +
fi
mkdir -p "${TARGET_DIR}"
# Replace preview directory contents for this branch.
find "${TARGET_DIR}" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
shopt -s nullglob
for item in "${PRIVATE_DOCS_DIR}"/*; do
  cp -R "${item}" "${TARGET_DIR}/"
done
shopt -u nullglob

# Fallback: if docs does not contain index.html, use web/index.html when available.
if [[ ! -f "${TARGET_DIR}/index.html" && -f "${PRIVATE_WEB_DIR}/index.html" ]]; then
  cp "${PRIVATE_WEB_DIR}/index.html" "${TARGET_DIR}/index.html"
fi

# Fallback: carry wasm_exec.js from web/ when docs does not provide it.
if [[ ! -f "${TARGET_DIR}/wasm_exec.js" && -f "${PRIVATE_WEB_DIR}/wasm_exec.js" ]]; then
  cp "${PRIVATE_WEB_DIR}/wasm_exec.js" "${TARGET_DIR}/wasm_exec.js"
fi

cache_buster="${PREVIEW_CACHE_BUSTER}"
if [[ -z "${cache_buster}" ]]; then
  cache_buster="$(date +%s)"
fi
if [[ -f "${TARGET_DIR}/index.html" ]]; then
  perl -0pi -e 's#wasm_exec\.js\?v=[^"'"'"'[:space:])]+#wasm_exec.js#g; s#game\.wasm\?v=[^"'"'"'[:space:])]+#game.wasm#g' "${TARGET_DIR}/index.html"
  perl -0pi -e 's#wasm_exec\.js#wasm_exec.js?v='"${cache_buster}"'#g; s#game\.wasm#game.wasm?v='"${cache_buster}"'#g' "${TARGET_DIR}/index.html"
fi

git add -A -f "${PREVIEW_ROOT}"
git add -f "${target_rel}"
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
  local owner_repo remote owner repo public_path
  if command -v gh >/dev/null 2>&1; then
    owner_repo="$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null || true)"
  fi
  if [[ -z "${owner_repo}" ]]; then
    remote="$(git config --get remote.origin.url || true)"
    owner_repo="$(printf '%s' "${remote}" | sed -E 's#^git@github.com:##; s#^https://github.com/##; s#\.git$##')"
  fi
  owner="${owner_repo%%/*}"
  repo="${owner_repo##*/}"
  public_path="${target_rel#docs/}"
  if [[ "${target_rel}" == "docs/latest" ]]; then
    printf 'https://%s.github.io/%s/' "${owner}" "${repo}"
  else
    printf 'https://%s.github.io/%s/%s/' "${owner}" "${repo}" "${public_path}"
  fi
}

if [[ -n "${CHATOPS_PREVIEW_URL_TEMPLATE:-}" ]]; then
  url="${CHATOPS_PREVIEW_URL_TEMPLATE}"
  url="${url//\{branch\}/${BRANCH}}"
  url="${url//\{branch_slug\}/${url_branch_slug}}"
  url="${url//\{preview_path\}/${target_rel#docs/}}"
  printf '%s\n' "${url}"
else
  build_default_url
fi
