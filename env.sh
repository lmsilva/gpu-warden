#!/usr/bin/env bash
# env.sh — runtime configuration for gpu-warden. SOURCE this, don't run it:
#
#     source ./env.sh
#
# Every value here is a default that warden reads as an environment fallback
# behind its command-line flags (flags for humans, env for deployment). Nothing
# in this file is machine-specific: the defaults describe the tunnels that
# scripts/dev-tunnels.sh opens, so a fresh clone works with no edits.
#
# To override, set the variable BEFORE sourcing, and it wins:
#     WARDEN_THRESHOLD=10 source ./env.sh
#
# This file is COMMITTED. It must never contain a credential — the Slurm JWT
# lives in .warden-env, which dev-tunnels.sh writes and .gitignore excludes.

# Refuse to be executed: exporting from a child process does nothing, and the
# silent no-op is a confusing way to lose ten minutes.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  printf 'env.sh must be sourced, not executed:\n\n    source ./env.sh\n\n' >&2
  exit 1
fi

# --- endpoints --------------------------------------------------------------
# Defaults match scripts/dev-tunnels.sh. In-cluster deployment overrides these
# with Service DNS names (see the Helm chart stretch goal).
: "${WARDEN_PROM_URL:=http://localhost:9090}"
: "${WARDEN_SLURM_URL:=http://localhost:6820}"
export WARDEN_PROM_URL WARDEN_SLURM_URL

# --- Slurm REST API version -------------------------------------------------
# Pinned, not "latest": v0.0.44 is what the slurm-operator itself talks. Older
# versions get retired (v0.0.42 is deprecated as of Slurm 26.05), so this is a
# deliberate choice. Confirm what your cluster serves:
#   curl -s -H "X-SLURM-USER-TOKEN: $SLURM_JWT" $WARDEN_SLURM_URL/openapi/v3 \
#     | grep -oE 'v0\.0\.[0-9]+' | sort -u
: "${WARDEN_SLURM_API:=v0.0.44}"
export WARDEN_SLURM_API

# --- the join key -----------------------------------------------------------
# Which DCGM label names the pod holding the GPU. Under kube-prometheus-stack
# this is "exported_pod": Prometheus's own target labels collide with the
# exporter's, so the scraped ones get prefixed and plain "pod" names the
# dcgm-exporter pod itself — the wrong one. Scraping the exporter directly
# yields plain "pod". Verify before trusting:
#   curl -s "$WARDEN_PROM_URL/api/v1/query?query=DCGM_FI_DEV_GPU_UTIL"
: "${WARDEN_POD_LABEL:=exported_pod}"
export WARDEN_POD_LABEL

# --- judgment parameters ----------------------------------------------------
# Sustained-peak threshold, in percent. A job whose peak utilization across ALL
# its GPUs stays at or below this for a full window is a zombie candidate. Set
# above zero as a noise floor: DCGM samples can register a percent or two from
# monitoring itself.
: "${WARDEN_THRESHOLD:=5}"

# The evaluation window. Long enough to survive checkpointing, dataloader
# stalls, and validation passes — the pauses that make an honest job look idle.
# Also the minimum age before ANY verdict is issued: a job younger than this
# has its query window clamped to its own age and is reported as too-young
# rather than judged.
: "${WARDEN_WINDOW:=15m}"

# How often warden re-evaluates. Comfortably longer than a Prometheus scrape
# interval; polling faster than data arrives only burns API calls.
: "${WARDEN_INTERVAL:=60s}"
export WARDEN_THRESHOLD WARDEN_WINDOW WARDEN_INTERVAL

# --- reporting --------------------------------------------------------------
# On-demand hourly cost of one GPU, for turning wasted GPU-hours into money.
# Default is g4dn.xlarge in us-west-2; change it to match your hardware, and
# say out loud that it's a list price, not your negotiated rate.
: "${WARDEN_DOLLAR_RATE:=0.526}"

# Port for warden's own /metrics endpoint.
: "${WARDEN_METRICS_PORT:=9500}"

# Namespace holding the Slurm worker pods.
: "${WARDEN_SLURM_NAMESPACE:=slurm}"
export WARDEN_DOLLAR_RATE WARDEN_METRICS_PORT WARDEN_SLURM_NAMESPACE

# --- credential -------------------------------------------------------------
# Written by scripts/dev-tunnels.sh; gitignored. Sourced here only so that one
# `source ./env.sh` leaves the shell fully ready.
if [ -f "$(dirname "${BASH_SOURCE[0]}")/.warden-env" ]; then
  # shellcheck source=/dev/null
  . "$(dirname "${BASH_SOURCE[0]}")/.warden-env"
fi

# --- summary ----------------------------------------------------------------
printf 'gpu-warden environment loaded\n'
printf '  prometheus   %s\n' "$WARDEN_PROM_URL"
printf '  slurmrestd   %s  (%s)\n' "$WARDEN_SLURM_URL" "$WARDEN_SLURM_API"
printf '  pod label    %s\n' "$WARDEN_POD_LABEL"
printf '  judgment     <=%s%% peak over %s, every %s\n' \
  "$WARDEN_THRESHOLD" "$WARDEN_WINDOW" "$WARDEN_INTERVAL"
if [ -n "${SLURM_JWT:-}" ]; then
  printf '  slurm token  present\n'
else
  printf '  slurm token  MISSING — run ./scripts/dev-tunnels.sh\n'
fi
