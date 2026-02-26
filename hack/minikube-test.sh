#!/usr/bin/env bash
# hack/minikube-test.sh
#
# End-to-end test of containerd-clone-snapshotter inside a minikube cluster.
#
# The script:
#   1. Starts a fresh minikube cluster with the containerd runtime.
#   2. Cross-compiles the snapshotter binary for linux/amd64.
#   3. Copies the binary into the minikube node and marks it executable.
#   4. Appends the proxy_plugins config to containerd's config.toml.
#   5. Starts the snapshotter daemon inside the node (via systemd unit).
#   6. Restarts containerd so it picks up the proxy plugin.
#   7. Deploys deploy/pod-source.yaml and waits for the pod to be Ready.
#   8. Discovers the source pod's containerd container ID (k8s.io namespace).
#   9. Uses ctr to prepare a clone snapshot.
#  10. Verifies the clone snapshot appears in the containerd snapshot list.
#  11. Cleans up (unless KEEP_CLUSTER=1 is set).
#
# Prerequisites
# ─────────────
#   minikube  >= 1.32   https://minikube.sigs.k8s.io/docs/start/
#   kubectl             https://kubernetes.io/docs/tasks/tools/
#   go        >= 1.21   https://go.dev/doc/install
#   docker              (used as the minikube driver)
#
# Environment variables
# ─────────────────────
#   MINIKUBE_PROFILE   minikube profile name  (default: clone-snapshotter-test)
#   KEEP_CLUSTER       set to 1 to leave the cluster running after the test
#   DRIVER             minikube driver        (default: docker)
#
# Usage
# ─────
#   bash hack/minikube-test.sh
#   KEEP_CLUSTER=1 bash hack/minikube-test.sh
#   MINIKUBE_PROFILE=my-test KEEP_CLUSTER=1 bash hack/minikube-test.sh

set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────────
PROFILE="${MINIKUBE_PROFILE:-clone-snapshotter-test}"
DRIVER="${DRIVER:-docker}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY="containerd-clone-snapshotter"
BINARY_PATH="${REPO_ROOT}/${BINARY}"
SOCKET_DIR="/run/containerd-clone-snapshotter"
SOCKET_PATH="${SOCKET_DIR}/${BINARY}.sock"
DATA_DIR="/var/lib/containerd-clone-snapshotter"
CLONE_KEY="clone-test-snapshot"
CONTAINERD_CONFIG="/etc/containerd/config.toml"
SERVICE_NAME="${BINARY}"

# ── Helpers ──────────────────────────────────────────────────────────────────
info()  { echo "[INFO]  $*"; }
ok()    { echo "[OK]    $*"; }
fail()  { echo "[FAIL]  $*" >&2; exit 1; }

mssh() {
    # Run a command inside the minikube node.
    minikube ssh -p "${PROFILE}" -- "$@"
}

# ── Prerequisite check ───────────────────────────────────────────────────────
info "Checking prerequisites..."
for cmd in minikube kubectl go docker; do
    command -v "${cmd}" >/dev/null 2>&1 || fail "'${cmd}' is not installed or not in PATH"
done
ok "All prerequisites satisfied."

# ── Cleanup trap ─────────────────────────────────────────────────────────────
cleanup() {
    local exit_code=$?
    if [[ "${KEEP_CLUSTER}" == "1" ]]; then
        info "KEEP_CLUSTER=1 — leaving minikube profile '${PROFILE}' running."
    else
        info "Deleting minikube profile '${PROFILE}'..."
        minikube delete -p "${PROFILE}" || true
    fi
    # Remove the locally built binary.
    rm -f "${BINARY_PATH}"
    if [[ ${exit_code} -eq 0 ]]; then
        ok "Test PASSED."
    else
        echo "[FAIL]  Test FAILED (exit ${exit_code})." >&2
    fi
}
trap cleanup EXIT

# ── 1. Start minikube ─────────────────────────────────────────────────────────
info "Starting minikube profile '${PROFILE}' (driver=${DRIVER}, runtime=containerd)..."
minikube start \
    -p "${PROFILE}" \
    --driver="${DRIVER}" \
    --container-runtime=containerd \
    --wait=all
ok "minikube is running."

# ── 2. Build the binary (linux/amd64) ────────────────────────────────────────
info "Building ${BINARY} for linux/amd64..."
cd "${REPO_ROOT}"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -trimpath -o "${BINARY_PATH}" ./cmd/containerd-clone-snapshotter
ok "Binary built at ${BINARY_PATH}."

# ── 3. Copy binary into the minikube node ────────────────────────────────────
info "Copying binary into minikube node..."
# minikube cp syntax: minikube cp -p PROFILE <local> <node>:<remote>
minikube cp -p "${PROFILE}" "${BINARY_PATH}" "${PROFILE}:/usr/local/bin/${BINARY}"
mssh sudo chmod +x "/usr/local/bin/${BINARY}"
ok "Binary installed at /usr/local/bin/${BINARY}."

# ── 4. Create socket/data directories ────────────────────────────────────────
info "Creating runtime directories inside the node..."
mssh sudo mkdir -p "${SOCKET_DIR}" "${DATA_DIR}"

# ── 5. Install a systemd unit for the snapshotter ────────────────────────────
info "Installing systemd unit for ${SERVICE_NAME}..."
mssh sudo tee /etc/systemd/system/${SERVICE_NAME}.service > /dev/null <<EOF
[Unit]
Description=containerd clone proxy snapshotter
After=network.target containerd.service
PartOf=containerd.service

[Service]
ExecStart=/usr/local/bin/${BINARY} \\
    -socket ${SOCKET_PATH} \\
    -root   ${DATA_DIR}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
mssh sudo systemctl daemon-reload
mssh sudo systemctl enable --now "${SERVICE_NAME}"
ok "systemd unit started."

# ── 6. Patch containerd config and restart containerd ────────────────────────
info "Patching containerd config (${CONTAINERD_CONFIG})..."

# Only append our stanza once.
if ! mssh grep -q "proxy_plugins.clone" "${CONTAINERD_CONFIG}" 2>/dev/null; then
    mssh sudo tee -a "${CONTAINERD_CONFIG}" > /dev/null <<EOF

# Added by hack/minikube-test.sh
[proxy_plugins]
  [proxy_plugins.clone]
    type    = "snapshot"
    address = "${SOCKET_PATH}"
EOF
    ok "proxy_plugins stanza added."
else
    ok "proxy_plugins.clone already present in config — skipping."
fi

info "Restarting containerd to pick up the proxy plugin..."
mssh sudo systemctl restart containerd

# Wait until the snapshotter socket appears (up to 30 s).
info "Waiting for snapshotter socket to appear..."
for i in $(seq 1 30); do
    if mssh test -S "${SOCKET_PATH}" 2>/dev/null; then
        ok "Socket is ready (${SOCKET_PATH})."
        break
    fi
    if [[ ${i} -eq 30 ]]; then
        mssh sudo journalctl -u "${SERVICE_NAME}" -n 30 --no-pager || true
        fail "Snapshotter socket did not appear within 30 s."
    fi
    sleep 1
done

# ── 7. Deploy source pod ──────────────────────────────────────────────────────
info "Deploying source pod (deploy/pod-source.yaml)..."
kubectl --context "${PROFILE}" apply -f "${REPO_ROOT}/deploy/pod-source.yaml"
info "Waiting for source-pod to be Running (up to 120 s)..."
kubectl --context "${PROFILE}" wait pod/source-pod \
    --for=condition=Ready --timeout=120s
ok "source-pod is Running."

# ── 8. Discover the source pod's container ID ────────────────────────────────
info "Discovering source-pod container ID in k8s.io namespace..."
SOURCE_CONTAINER_ID=$(
    mssh sudo ctr -n k8s.io containers ls \
        'labels."io.kubernetes.pod.name"==source-pod' \
        -q 2>/dev/null | head -1
)
if [[ -z "${SOURCE_CONTAINER_ID}" ]]; then
    fail "Could not find source-pod container in k8s.io namespace."
fi
ok "Source container ID: ${SOURCE_CONTAINER_ID}"

# ── 9. Prepare the clone snapshot ────────────────────────────────────────────
info "Preparing clone snapshot '${CLONE_KEY}' from ${SOURCE_CONTAINER_ID}..."
mssh sudo ctr -n k8s.io snapshots \
    --snapshotter clone \
    prepare \
    --label "containerd.io/snapshot/clone-source=${SOURCE_CONTAINER_ID}" \
    "${CLONE_KEY}" ""
ok "Clone snapshot prepared."

# ── 10. Verify ───────────────────────────────────────────────────────────────
info "Verifying clone snapshot exists..."
SNAP_LINE=$(mssh sudo ctr -n k8s.io snapshots ls 2>/dev/null | grep "${CLONE_KEY}" || true)
if [[ -z "${SNAP_LINE}" ]]; then
    fail "Clone snapshot '${CLONE_KEY}' not found in snapshot list."
fi
ok "Clone snapshot found: ${SNAP_LINE}"

echo ""
echo "══════════════════════════════════════════════════════"
echo "  containerd-clone-snapshotter minikube test: PASSED"
echo "══════════════════════════════════════════════════════"
