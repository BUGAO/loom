#!/usr/bin/env bash
# loom installer — dependencies and a PATH-visible binary.
#
#   ./scripts/install.sh
#
# Layout after install (all user-level, no sudo anywhere):
#
#   ~/.local/bin/loom     THE binary, on PATH (rebuilt in place on reinstall)
#   ~/.loom/acp/          claude-code-acp adapter (npm, stable across checkouts)
#   ~/.loom/data/         default data dir (agents, workflows, runs)
#
# No launcher script: the binary itself defaults to ~/.loom/data and finds the
# adapter under ~/.loom/acp, so running `loom` from any directory hits the
# same store. Process management is built in too: loom start|stop|restart|status.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOOM_HOME="${LOOM_HOME:-$HOME/.loom}"
BIN_DIR="$LOOM_HOME/bin"
ACP_DIR="$LOOM_HOME/acp"
DATA_DIR="$LOOM_HOME/data"
LAUNCHER_DIR="$HOME/.local/bin"
LAUNCHER="$LAUNCHER_DIR/loom"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ---- 1. dependencies ----

command -v go >/dev/null 2>&1 \
  || die "Go is required (go.mod wants the version pinned there). Install: https://go.dev/dl/ or 'brew install go'"

GO_WANT="$(awk '/^go /{print $2}' "$REPO_ROOT/go.mod")"
info "Go $(go version | awk '{print $3}') found (go.mod wants go${GO_WANT})"

if command -v npm >/dev/null 2>&1; then
  info "installing ACP adapter (claude-code-acp) into $ACP_DIR"
  mkdir -p "$ACP_DIR"
  npm install --prefix "$ACP_DIR" --no-fund --no-audit --loglevel=error @zed-industries/claude-code-acp
else
  warn "npm not found — skipping the ACP adapter. Static workflows degrade to CLI single-shot;"
  warn "dynamic (conversational) workflows will refuse to start. Install Node.js, then re-run."
fi

command -v claude >/dev/null 2>&1 \
  || warn "Claude Code CLI ('claude') not found on PATH — the planner and real runs need it; dry-run works without."

# ---- 2. build straight onto PATH ----

mkdir -p "$LAUNCHER_DIR" "$DATA_DIR"
info "building loom → $LAUNCHER"
# go build refuses to overwrite a non-object file — clear the legacy shell
# launcher (or any stray file) so the binary can take its place.
rm -f "$LAUNCHER"
(cd "$REPO_ROOT" && go build -o "$LAUNCHER" ./cmd/loom)

# Legacy layout cleanup: earlier installs kept the binary in ~/.loom/bin and a
# shell launcher on PATH (just overwritten by the build above). The binary now
# carries its own defaults (data: ~/.loom/data, adapter: ~/.loom/acp).
rm -f "$BIN_DIR/loom"
rmdir "$BIN_DIR" 2>/dev/null || true

# First install: carry over a repo-local ./data so existing agents, workflows
# and run history don't appear to vanish when 'loom' starts using ~/.loom/data.
if [ -d "$REPO_ROOT/data" ] && [ -z "$(ls -A "$DATA_DIR" 2>/dev/null)" ]; then
  info "migrating existing repo data ($REPO_ROOT/data) → $DATA_DIR"
  cp -R "$REPO_ROOT/data/." "$DATA_DIR/"
fi
info "installed $LAUNCHER (data: $DATA_DIR)"

case ":$PATH:" in
  *":$LAUNCHER_DIR:"*) ;;
  *)
    warn "$LAUNCHER_DIR is not on your PATH. Add this to your shell profile (~/.zshrc):"
    printf '\n    export PATH="$HOME/.local/bin:$PATH"\n\n'
    ;;
esac

info "done. Run:  loom start      (background, http://localhost:7333)"
info "also:  loom restart | stop | status   |   loom (foreground)   |   flags: -dry-run -addr -data"
