#!/usr/bin/env bash
# dev-tunnels.sh — tear down and re-establish the three port-forwards
# squire needs, then mint a fresh Slurm JWT.
#
#   ./scripts/dev-tunnels.sh          start (or restart) everything
#   ./scripts/dev-tunnels.sh --stop   tear the tunnels down and exit
#
# Tunnels opened:
#   localhost:6820  -> svc/slurm-restapi          (slurmrestd, namespace slurm)
#   localhost:9090  -> Prometheus                 (namespace prometheus)
#   localhost:3000  -> Grafana                    (namespace prometheus)
#
set -uo pipefail

SLURM_NS="${SLURM_NS:-slurm}"
PROM_NS="${PROM_NS:-prometheus}"
PROM_SVC="${PROM_SVC:-prometheus-kube-prometheus-prometheus}"
RESTAPI_SVC="${RESTAPI_SVC:-slurm-restapi}"
CTLD_POD="${CTLD_POD:-slurm-controller-0}"

RUN_DIR="${TMPDIR:-/tmp}/squire"
ENV_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/.squire-env"
mkdir -p "$RUN_DIR"

red()  { printf '\033[31m%s\033[0m\n' "$*"; }
grn()  { printf '\033[32m%s\033[0m\n' "$*"; }
ylw()  { printf '\033[33m%s\033[0m\n' "$*"; }

# --- teardown ---------------------------------------------------------------
# Match on the port-forward argument so we only kill OUR tunnels, never an
# unrelated kubectl the user is running.
stop_tunnels() {
  local port killed=0
  for port in 6820 9090 3000; do
    if pkill -f "kubectl.*port-forward.*:${port}" 2>/dev/null; then
      killed=1
    fi
  done
  # Older invocations may have used the bare "3000" form (no local:remote pair).
  pkill -f "kubectl.*port-forward.*grafana" 2>/dev/null && killed=1
  [ "$killed" -eq 1 ] && sleep 1
  rm -f "$RUN_DIR"/*.pid 2>/dev/null
  return 0
}

if [ "${1:-}" = "--stop" ]; then
  stop_tunnels
  grn "Tunnels stopped."
  exit 0
fi

# --- preflight --------------------------------------------------------------
command -v kubectl >/dev/null || { red "kubectl not found in PATH."; exit 1; }

if ! kubectl cluster-info >/dev/null 2>&1; then
  red "Cannot reach the cluster. Check your kubeconfig / AWS credentials."
  exit 1
fi

ylw "Stopping any existing tunnels..."
stop_tunnels

# --- helper: start one port-forward and wait for the port to answer ----------
start_forward() {
  local name="$1" ns="$2" target="$3" ports="$4"
  local log="$RUN_DIR/${name}.log"

  kubectl -n "$ns" port-forward "$target" "$ports" >"$log" 2>&1 &
  local pid=$!
  echo "$pid" >"$RUN_DIR/${name}.pid"

  local local_port="${ports%%:*}"
  for _ in $(seq 1 25); do
    if ! kill -0 "$pid" 2>/dev/null; then
      red "  $name: port-forward exited. Last lines:"
      tail -n 3 "$log" | sed 's/^/      /'
      return 1
    fi
    # bash's /dev/tcp is a builtin — no nc dependency.
    if (exec 3<>"/dev/tcp/127.0.0.1/${local_port}") 2>/dev/null; then
      exec 3<&- 3>&-
      grn "  $name  -> localhost:${local_port}  (pid $pid)"
      return 0
    fi
    sleep 0.4
  done
  red "  $name: port ${local_port} never opened. Log: $log"
  return 1
}

# --- the three tunnels ------------------------------------------------------
echo
ylw "Starting tunnels..."
failed=0

start_forward "slurmrestd" "$SLURM_NS" "svc/${RESTAPI_SVC}" "6820:6820" || failed=1
start_forward "prometheus" "$PROM_NS"  "svc/${PROM_SVC}"    "9090:9090" || failed=1

# Grafana's Service port differs by chart version, so target the pod directly:
# a pod's container port is unambiguous.
GRAFANA_POD="$(kubectl -n "$PROM_NS" get pod \
  -l "app.kubernetes.io/name=grafana,app.kubernetes.io/instance=prometheus" \
  -o name 2>/dev/null | head -n 1)"

if [ -n "$GRAFANA_POD" ]; then
  start_forward "grafana" "$PROM_NS" "$GRAFANA_POD" "3000:3000" || failed=1
else
  ylw "  grafana: no pod found in namespace ${PROM_NS} — skipping."
fi

# --- Slurm JWT --------------------------------------------------------------
echo
ylw "Minting a 24h Slurm JWT for ${TOKEN_USER:-root}..."
#   username=root because we need the ability to view every job and username=slurm
#     (usually "slurm") is rejected by slurmrestd on real endpoints: that
#     identity owns the service.
#
TOKEN_USER="${TOKEN_USER:-root}"
TOKEN_RAW="$(kubectl -n "$SLURM_NS" exec "$CTLD_POD" -c slurmctld -- \
  scontrol token username="$TOKEN_USER" lifespan=86400 2>/dev/null | tr -d '\r')"
SLURM_JWT="${TOKEN_RAW#SLURM_JWT=}"

if [ -n "$SLURM_JWT" ] && [ "$SLURM_JWT" != "$TOKEN_RAW" ]; then
  printf 'export SLURM_JWT=%s\n' "$SLURM_JWT" >"$ENV_FILE"
  chmod 600 "$ENV_FILE"
  grn "  token written to $ENV_FILE"
else
  red "  Could not mint a token (is $CTLD_POD running?). Tunnels are still up."
fi

# --- verify the API actually answers ----------------------------------------
if [ -n "${SLURM_JWT:-}" ]; then
  echo
  ylw "Verifying slurmrestd..."
  # Deliberately /jobs, not /ping: ping answers without authentication, so a
  # green ping proves only that the tunnel is open. /jobs is the first endpoint
  # that actually checks the token.
  if curl -sf -H "X-SLURM-USER-TOKEN: ${SLURM_JWT}" \
       "http://localhost:6820/slurm/${SLURM_API:-v0.0.44}/jobs" >/dev/null 2>&1; then
    grn "  authenticated query OK (API ${SLURM_API:-v0.0.44})"
  else
    ylw "  authenticated query failed. If this cluster serves a different API version:"
    echo "      SLURM_API=v0.0.45 ./scripts/dev-tunnels.sh"
    echo "  List what it actually serves:"
    echo "      curl -s -H \"X-SLURM-USER-TOKEN: \$SLURM_JWT\" http://localhost:6820/openapi/v3 | grep -o '/slurm/v0\\.0\\.[0-9]*' | sort -u"
  fi
fi

# --- summary ----------------------------------------------------------------
echo
if [ "$failed" -eq 0 ]; then
  grn "Ready."
else
  ylw "Some tunnels failed — see messages above."
fi
cat <<EOF

  Load the token into THIS shell:   source .squire-env
  Prometheus UI:                    http://localhost:9090
  Grafana UI:                       http://localhost:3000  (user: admin)
  Grafana password:
      kubectl -n ${PROM_NS} get secret prometheus-grafana \\
        -o jsonpath="{.data.admin-password}" | base64 -d ; echo

  Stop everything:                  ./scripts/dev-tunnels.sh --stop
  Tunnel logs:                      ${RUN_DIR}/

EOF
