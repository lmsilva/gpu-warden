#!/usr/bin/env bash
# setup.sh — verify this machine can build and run gpu-warden, then warm the
# module cache. Safe to re-run; it changes nothing outside the module cache.
#
#   ./scripts/setup.sh
#
# What this does NOT do: install a Go toolchain, configure kubectl, or set up
# the cluster. Those are machine-level concerns deliberately left outside the
# repo — see README. This script's job is to tell you, in one run, exactly
# which prerequisite is missing.

set -uo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1

red() { printf '\033[31m%s\033[0m\n' "$*"; }
grn() { printf '\033[32m%s\033[0m\n' "$*"; }
ylw() { printf '\033[33m%s\033[0m\n' "$*"; }

MIN_GO_MINOR=21   # generics + slices/maps stdlib packages
problems=0

echo "gpu-warden setup check"
echo "  repo: $(pwd)"
echo

# --- Go toolchain -----------------------------------------------------------
if ! command -v go >/dev/null 2>&1; then
  red "  go        not found in PATH"
  echo "            Install it once, system-wide, so no project depends on a"
  echo "            per-folder environment script:"
  echo "              curl -sL https://go.dev/dl/go1.24.0.linux-amd64.tar.gz | sudo tar -C /usr/local -xz"
  echo "              echo 'export PATH=\$PATH:/usr/local/go/bin' >> ~/.bashrc"
  problems=$((problems + 1))
else
  GO_VER="$(go version | awk '{print $3}')"        # e.g. go1.24.0
  GO_MINOR="$(printf '%s' "$GO_VER" | sed -E 's/^go1\.([0-9]+).*/\1/')"
  if [[ "$GO_MINOR" =~ ^[0-9]+$ ]] && [ "$GO_MINOR" -lt "$MIN_GO_MINOR" ]; then
    red "  go        $GO_VER  (need 1.${MIN_GO_MINOR} or newer)"
    problems=$((problems + 1))
  else
    grn "  go        $GO_VER"
  fi
fi

# --- kubectl (needed at runtime, not to build) ------------------------------
if command -v kubectl >/dev/null 2>&1; then
  grn "  kubectl   present"
  if kubectl cluster-info >/dev/null 2>&1; then
    grn "  cluster   reachable ($(kubectl config current-context 2>/dev/null))"
  else
    ylw "  cluster   unreachable — fine for building, required to run warden"
  fi
else
  ylw "  kubectl   not found — fine for building, required to run warden"
fi

# --- module cache -----------------------------------------------------------
echo
if [ -f go.mod ]; then
  ylw "Downloading module dependencies (cached globally, shared across projects)..."
  if go mod download 2>&1 | sed 's/^/  /'; then
    grn "  dependencies ready"
  else
    red "  go mod download failed — see output above"
    problems=$((problems + 1))
  fi

  # Only vet if there is something to vet; an empty scaffold is not an error.
  if compgen -G "**/*.go" >/dev/null 2>&1 || ls ./*.go >/dev/null 2>&1; then
    if go vet ./... 2>&1 | sed 's/^/  /'; then
      grn "  go vet clean"
    else
      ylw "  go vet reported issues (above) — not fatal for setup"
    fi
  fi
else
  red "  go.mod not found. Run this from the repo, after:"
  echo "      go mod init github.com/lmsilva/gpu-warden"
  problems=$((problems + 1))
fi

# --- summary ----------------------------------------------------------------
echo
if [ "$problems" -eq 0 ]; then
  grn "Setup OK."
  cat <<'EOF'

  Next:
    ./scripts/dev-tunnels.sh && source .warden-env    open tunnels, mint a JWT
    go run ./cmd/warden --help                        confirm the binary builds

EOF
else
  red "$problems problem(s) above. Fix those, then re-run."
  exit 1
fi
