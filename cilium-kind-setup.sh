#!/usr/bin/env bash
# Recreate the local "kind" cluster with Cilium as the CNI (default kindnet
# disabled) + Hubble, preserving the node setup Agent Substrate needs
# (ClusterTrustBundle feature gates, proxy_arp for sandbox pod networking, the
# localhost:5001 registry, parallel image pulls). gVisor only; no KVM/micro-VM.
#
# Mirrors hack/create-kind-cluster.sh's node setup, with disableDefaultCNI.
set -o errexit -o nounset -o pipefail

CLUSTER="${KIND_CLUSTER_NAME:-kind}"
CTX="kind-${CLUSTER}"
REG_NAME="kind-registry"
REG_PORT="5001"
CFG="/Users/doniacld/correlation-poc/kind-cilium-config.yaml"

echo "### 1. delete existing cluster ${CLUSTER} (if any)"
kind delete cluster --name "${CLUSTER}" || true

echo "### 2. ensure local registry ${REG_NAME} on :${REG_PORT}"
if [ "$(docker inspect -f '{{.State.Running}}' "${REG_NAME}" 2>/dev/null || true)" != "true" ]; then
  docker run -d --restart=always --label created-by=agent-substrate \
    -p "127.0.0.1:${REG_PORT}:5000" --network bridge --name "${REG_NAME}" registry:3
fi

echo "### 3. write kind config (disableDefaultCNI + Substrate feature gates)"
cat > "${CFG}" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
featureGates:
  ClusterTrustBundle: true
  ClusterTrustBundleProjection: true
  PodCertificateRequest: true
runtimeConfig:
  "certificates.k8s.io/v1beta1": "true"
networking:
  ipFamily: ipv4
  disableDefaultCNI: true    # Cilium will be the CNI
kubeadmConfigPatches:
- |
  kind: KubeletConfiguration
  serializeImagePulls: false
  maxParallelImagePulls: 4
EOF

echo "### 4. create cluster"
kind create cluster --name "${CLUSTER}" --config "${CFG}"

echo "### 5. proxy_arp on nodes (sandbox pod networking)"
for node in $(kind get nodes --name "${CLUSTER}"); do
  docker exec "${node}" sysctl net.ipv4.conf.all.proxy_arp=1
done

echo "### 6. registry wiring on nodes"
REGISTRY_DIR="/etc/containerd/certs.d/localhost:${REG_PORT}"
for node in $(kind get nodes --name "${CLUSTER}"); do
  docker exec "${node}" mkdir -p "${REGISTRY_DIR}"
  echo "[host.\"http://${REG_NAME}:5000\"]" | docker exec -i "${node}" cp /dev/stdin "${REGISTRY_DIR}/hosts.toml"
done
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${REG_NAME}")" = "null" ]; then
  docker network connect "kind" "${REG_NAME}"
fi
kubectl --context="${CTX}" apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${REG_PORT}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

echo "### 7. install Cilium + Hubble (kube-proxy coexist)"
cilium install \
  --set hubble.relay.enabled=true \
  --set hubble.ui.enabled=true

echo "### 8. wait for Cilium + Hubble"
cilium status --wait --wait-duration 5m

echo "### 9. nodes Ready?"
kubectl --context="${CTX}" get nodes -o wide
echo "DONE"
