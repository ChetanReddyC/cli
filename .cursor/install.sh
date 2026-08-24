#!/usr/bin/env bash
# Cloud Agent install script for the Entire CLI.
#
# Idempotent: safe to run repeatedly and against a cached/partially-prepared
# snapshot. It prepares the mise-pinned toolchain, downloads Go modules, and
# builds the CLI. It does NOT start any long-running process.
set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

log() { printf '\n\033[1m[install]\033[0m %s\n' "$*"; }

# 1. Ensure git >= 2.45. The test suite creates a reftable repository with
#    `git init --ref-format=reftable` (ref-format was stabilised in git 2.45),
#    and Ubuntu 24.04 ships 2.43. Upgrade from the git-core PPA when needed.
git_supports_reftable() {
  local v major minor
  v="$(git --version | awk '{print $3}')"
  major="${v%%.*}"
  minor="$(printf '%s' "$v" | cut -d. -f2)"
  [ "${major}" -gt 2 ] || { [ "${major}" -eq 2 ] && [ "${minor}" -ge 45 ]; }
}

if ! git_supports_reftable; then
  log "Upgrading git (need >= 2.45 for reftable support; have $(git --version | awk '{print $3}'))"
  sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends software-properties-common
  sudo add-apt-repository -y ppa:git-core/ppa
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --only-upgrade git
fi
log "git $(git --version | awk '{print $3}')"

# 2. Ensure mise (task runner + toolchain manager) is installed and on PATH.
if ! command -v mise >/dev/null 2>&1; then
  if [ ! -x "${HOME}/.local/bin/mise" ]; then
    log "Installing mise"
    curl -fsSL https://mise.run | sh
  fi
  export PATH="${HOME}/.local/bin:${PATH}"
fi
log "mise $(mise --version)"

# 3. Make mise + its shims available in interactive agent terminals.
BASHRC="${HOME}/.bashrc"
if [ -w "${BASHRC}" ] || [ ! -e "${BASHRC}" ]; then
  if ! grep -q 'mise activate bash' "${BASHRC}" 2>/dev/null; then
    cat >> "${BASHRC}" <<'MISE_ACTIVATE'
export PATH="$HOME/.local/bin:$PATH"
command -v mise >/dev/null 2>&1 && eval "$(mise activate bash)"
MISE_ACTIVATE
  fi
fi

# 4. Install the pinned toolchain (Go, golangci-lint, gotestsum, shellcheck,
#    tmux, and the roger-roger E2E helper binaries) declared in mise.toml.
mise trust --yes
mise install
eval "$(mise activate bash --shims)"

# 5. Download Go modules and build the CLI to prove the toolchain works.
log "Downloading Go modules"
go mod download

log "Building the entire CLI"
go build -o entire ./cmd/entire/
./entire version

# 6. Put `entire` on PATH and wire Entire's git hooks so this repo's committed
#    hooks fire while an agent works here (Entire dogfoods itself on this repo).
log "Wiring entire onto PATH and installing git hooks"
bash "${REPO_ROOT}/.cursor/wire-entire-hooks.sh" || true

log "Environment ready."
