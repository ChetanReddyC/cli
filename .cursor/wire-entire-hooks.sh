#!/usr/bin/env bash
# Make the freshly built `entire` binary usable for Entire's own hooks in this
# repo (Entire dogfoods itself here). This repo commits agent hook configs
# (.cursor/hooks.json, .claude/settings.json, ...) and Entire git hooks that all
# invoke the bare `entire` command and no-op when it is not on PATH. So we:
#
#   1. put `entire` on PATH where hook execution can find it, and
#   2. wire Entire's git hooks into the active git hooksPath.
#
# Idempotent and best-effort: it never fails its caller.
set -uo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}" || exit 0

BIN="${REPO_ROOT}/entire"
if [ ! -x "${BIN}" ]; then
  echo "[wire-entire-hooks] ${BIN} not built yet; skipping" >&2
  exit 0
fi

# 1. Put `entire` on PATH. Hooks (Cursor agent hooks and git hooks) run with a
#    standard system PATH, so prefer /usr/local/bin; fall back to ~/.local/bin
#    when sudo is unavailable.
if sudo -n ln -sf "${BIN}" /usr/local/bin/entire 2>/dev/null; then
  :
else
  mkdir -p "${HOME}/.local/bin"
  ln -sf "${BIN}" "${HOME}/.local/bin/entire"
fi

# 2. Install Entire's git hooks into the active hooksPath. Cursor manages
#    core.hooksPath in agent VMs and regenerates it per boot, so this is re-run
#    from `start` too. `configure --force` re-serializes .entire/settings.json;
#    restore the original bytes (mtime included) so the committed file is never
#    churned in the working tree.
if command -v entire >/dev/null 2>&1; then
  settings=".entire/settings.json"
  backup="$(mktemp)"
  [ -f "${settings}" ] && cp -p "${settings}" "${backup}"
  entire configure --force >/dev/null 2>&1 || true
  if [ -s "${backup}" ]; then cp -p "${backup}" "${settings}"; fi
  rm -f "${backup}"
fi

echo "[wire-entire-hooks] entire on PATH: $(command -v entire || echo MISSING)"
