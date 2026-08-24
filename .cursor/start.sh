#!/usr/bin/env bash
# Per-boot reconciliation for the Entire CLI dev environment.
#
# There are no long-running services. Cursor regenerates the git hooksPath on
# each boot, so re-wire Entire's hooks (and re-assert `entire` on PATH) every
# time the environment starts. Best-effort: it must not block startup.
set -uo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

bash "${REPO_ROOT}/.cursor/wire-entire-hooks.sh" || true
