#!/bin/sh
# hivemind installer — POSIX, idempotent, re-runnable.
# Supports `curl … | sh` and `./install.sh` equally.
#
#   ./install.sh                 # install prebuilt binary for this OS/arch (when released)
#   ./install.sh --from-source   # go build from the current source tree
#   ./install.sh --systemd       # also install a systemd unit that runs `hivemind up` on boot
#
# NOTE: M1. The prebuilt-download path points at a release URL that does not exist
# yet; use --from-source until releases are published. The systemd unit is a
# convenience and is fully realized in M3.
set -eu

PREFIX="${PREFIX:-/usr/local/bin}"
RELEASE_BASE="${HIVEMIND_RELEASE_BASE:-https://example.invalid/hivemind/releases/latest}"
FROM_SOURCE=0
WITH_SYSTEMD=0

for arg in "$@"; do
  case "$arg" in
    --from-source) FROM_SOURCE=1 ;;
    --systemd)     WITH_SYSTEMD=1 ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

say()  { printf '  %s\n' "$*"; }
warn() { printf '  ! %s\n' "$*" >&2; }
die()  { printf '  ✗ %s\n' "$*" >&2; exit 1; }

# --- detect OS/arch ---
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported arch: $arch" ;;
esac
say "platform: ${os}/${arch}"

# --- dependency checks ---
if command -v claude >/dev/null 2>&1; then
  say "claude: $(command -v claude)"
else
  warn "claude CLI not found — install it (https://docs.claude.com/claude-code). hivemind needs it at runtime (or use --fake for dry runs)."
fi
if command -v tmux >/dev/null 2>&1; then
  say "tmux: $(command -v tmux)"
else
  die "tmux not found — required to run service tools. Install tmux and re-run."
fi
if command -v docker >/dev/null 2>&1; then
  say "docker: $(command -v docker) (optional)"
else
  warn "docker not found (optional; only needed by configs that use it)."
fi

# --- choose install method ---
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# If we're sitting in the source tree and no release exists yet, prefer building
# from source automatically rather than attempting a download of a nonexistent
# release. (Pass --from-source to force it explicitly anywhere.)
if [ "$FROM_SOURCE" -eq 0 ] && [ -f "$here/go.mod" ] && command -v go >/dev/null 2>&1; then
  say "source tree detected — building from source (no published release yet)."
  FROM_SOURCE=1
fi

# --- install the binary ---
tmpbin=""
if [ "$FROM_SOURCE" -eq 1 ]; then
  command -v go >/dev/null 2>&1 || die "go toolchain not found (needed to build from source)."
  say "building from source…"
  ( cd "$here" && GOTOOLCHAIN=local go build -o "$here/hivemind" . ) || die "go build failed."
  tmpbin="$here/hivemind"
else
  url="${RELEASE_BASE}/hivemind_${os}_${arch}"
  say "downloading ${url}…"
  tmpbin=$(mktemp)
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$tmpbin" || die "download failed. Try: ./install.sh --from-source"
  else
    wget -qO "$tmpbin" "$url" || die "download failed. Try: ./install.sh --from-source"
  fi
  chmod +x "$tmpbin"
fi

dest="${PREFIX}/hivemind"
if [ -w "$PREFIX" ]; then
  install -m 0755 "$tmpbin" "$dest"
else
  say "writing $dest (sudo)…"
  sudo install -m 0755 "$tmpbin" "$dest"
fi
say "installed: $dest"

# --- global state dir ---
mkdir -p "${HOME}/.hivemind"
say "state dir: ${HOME}/.hivemind"

# --- optional systemd unit ---
if [ "$WITH_SYSTEMD" -eq 1 ]; then
  if command -v systemctl >/dev/null 2>&1; then
    unit=/etc/systemd/system/hivemind.service
    say "installing systemd unit $unit (sudo)…"
    sudo sh -c "cat > '$unit'" <<EOF
[Unit]
Description=hivemind agent fleet
After=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=${dest} up
WorkingDirectory=%h

[Install]
WantedBy=multi-user.target
EOF
    sudo systemctl daemon-reload
    say "enable with: sudo systemctl enable --now hivemind"
  else
    warn "systemctl not found; skipping systemd unit."
  fi
fi

printf '\n  Next step: hivemind setup\n'
