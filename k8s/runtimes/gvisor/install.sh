#!/usr/bin/env bash
# Install gVisor (runsc) on a k3s node and register it with k3s' containerd.
#
# Run as root on each node:   sudo ./install.sh
#
# Decisions:
#  - Pinned release + SHA-512 verification (no "latest", no curl|bash).
#  - gvisor-bin/ (sentry + sidecars) is installed next to runsc explicitly.
#    Otherwise runsc downloads the sidecars itself at first start
#    (--sidecar-download=IF_RELEASE_BUILD), an unverified fetch on the node.
#  - Registered through k3s' containerd drop-in dir (config-v3.toml.d), not by
#    overriding k3s' config template, so k3s upgrades and kata-deploy (which
#    uses the same drop-in dir) keep working.
set -euo pipefail

GVISOR_RELEASE="${GVISOR_RELEASE:-20261019}"
ARCH="$(uname -m)"
BASE_URL="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE}/${ARCH}"
PREFIX=/usr/local/bin
K3S_CTD_DIR=/var/lib/rancher/k3s/agent/etc/containerd
DROPIN_DIR="${K3S_CTD_DIR}/config-v3.toml.d"
HERE="$(cd "$(dirname "$0")" && pwd)"

[[ $EUID -eq 0 ]] || { echo "must run as root" >&2; exit 1; }

# The rendered k3s config must import the drop-in dir (k3s >= 1.31 w/ containerd 2.x).
grep -q 'config-v3.toml.d' "${K3S_CTD_DIR}/config.toml" || {
  echo "k3s containerd config does not import ${DROPIN_DIR}; unsupported k3s version" >&2
  exit 1
}

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
curl -fsSLO "${BASE_URL}/gvisor.tar.bz2"
curl -fsSLO "${BASE_URL}/gvisor.tar.bz2.sha512"
sha512sum -c gvisor.tar.bz2.sha512
python3 -I -c "import tarfile; tarfile.open('gvisor.tar.bz2','r:bz2').extractall('x', filter='data')"

install -m 0755 x/runsc x/containerd-shim-runsc-v1 "${PREFIX}/"
rm -rf "${PREFIX}/gvisor-bin"
cp -r x/gvisor-bin "${PREFIX}/gvisor-bin"
chown -R root:root "${PREFIX}/gvisor-bin"
chmod -R a+rX,go-w "${PREFIX}/gvisor-bin"
"${PREFIX}/runsc" --version

install -d -m 0755 /etc/gvisor
install -m 0644 "${HERE}/runsc.toml" /etc/gvisor/runsc.toml

install -d -m 0755 "${DROPIN_DIR}"
install -m 0644 "${HERE}/containerd-gvisor.toml" "${DROPIN_DIR}/10-gvisor.toml"

systemctl restart k3s
echo "gVisor ${GVISOR_RELEASE} installed; apply runtimeclass.yaml next."
