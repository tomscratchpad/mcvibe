# Sandbox runtimes: gVisor and Kata Containers

Agent pods must not share the host kernel with each other or with the node. The
cluster offers two hardened RuntimeClasses next to the default `runc`:

| RuntimeClass | Handler                | Boundary                                         | Cost / constraints                                   |
|--------------|------------------------|--------------------------------------------------|------------------------------------------------------|
| *(none)*     | runc                   | Linux namespaces + cgroups, **shared host kernel** | Fastest; one kernel bug = node compromise            |
| `gvisor`     | runsc (systrap)        | User-space kernel (Sentry) intercepts syscalls     | No KVM needed, low overhead; syscall compat gaps, slower I/O |
| `kata`       | kata-qemu-runtime-rs   | Lightweight VM per pod, **own guest kernel**       | Needs `/dev/kvm` (nested virt here); ~600 MiB idle overhead, ~5 s boot per pod |

Agent sandboxes select one with `spec.podTemplate.spec.runtimeClassName`.
Admission policy allows only `gvisor` or `kata` for agent sessions (runc is rejected).

**Default: `gvisor`**, for performance and density: about 0.6 s cold start versus
about 5 s for Kata, tens of MiB versus about 600 MiB per session, and no KVM
requirement. **Kata stays installed** for workloads that need a
hardware-virtualization boundary. Its template is
`../policies/examples/sandboxtemplate-claude-code-kata.yaml`. The full rationale
is in [../policies/README.md](../policies/README.md#runtime-choice-gvisor-kata-available).

## Install

```shell
# gVisor: host binaries + containerd drop-in, needs root on each node
sudo gvisor/install.sh
kubectl apply -f gvisor/runtimeclass.yaml

# Kata: upstream kata-deploy Helm chart (per-node install Jobs)
kata/install.sh
```

### Decisions

- **Pinned + verified artifacts.** gVisor release `20261019` (SHA-512 checked),
  kata-deploy chart/images `4.3.1`. No `latest`, no `curl | bash`.
- **gVisor sidecars installed explicitly.** Recent `runsc` needs `gvisor-bin/`
  next to it and would otherwise *download it at first sandbox start*. The script
  installs it from the verified tarball, so the node never fetches binaries at runtime.
- **containerd drop-in, not template override.** k3s (containerd 2.x) imports
  `/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d/*.toml`. gVisor
  adds `10-gvisor.toml` there and kata-deploy uses the same mechanism, so neither
  owns k3s' config template and k3s upgrades keep working.
- **kata-deploy in `job` mode, single shim.** Install runs as one-shot privileged
  Jobs (no resident privileged DaemonSet). Only `qemu-runtime-rs` (the 4.x default)
  is enabled, which keeps the node footprint down.
- **gVisor platform `systrap`** works in a VM without KVM. On bare metal with
  KVM, `platform = "kvm"` is faster.

## Verify

```shell
test/verify.sh          # KEEP=1 to leave the sandboxes running
```

Result on the demo VM (host kernel `6.17.0-41-generic`):

```
=== rt-runc   (runtimeClassName=<none>)   kernel : 6.17.0-41-generic   <- host kernel
=== rt-gvisor (runtimeClassName=gvisor)   kernel : 4.19.0-gvisor       dmesg: Starting gVisor...
=== rt-kata   (runtimeClassName=kata)     kernel : 6.18.56             <- guest VM kernel
```

## Operational notes / lessons from the demo

- **Disk.** The Kata install (images plus about 830 MB in `/opt/kata`) pushed a 25 GB
  node into `DiskPressure`. Kubelet then evicted the agent-sandbox controller in a
  loop (about 85 evicted pods). The VM disk was grown to 100 GB. In production:
  size node disks for runtime artifacts and image cache, and alert on
  `DiskPressure` before eviction.
- **Evicted sandboxes are not recreated.** The `Sandbox` whose pod was evicted
  stays `PodSucceeded`. Expected for disposable sessions, but the platform must
  surface this to the engineer, since their session state lives on that pod.
- **k3s restarts rewrite `/etc/rancher/k3s/k3s.yaml`** (root-only). Use a copy at
  `~/.kube/config`, or set `write-kubeconfig-mode` in k3s config.
- **Port-forwarding** to gVisor and Kata pods is not supported. Remote access goes
  through `kubectl exec` or the agent-sandbox Sandbox Router.
- **Runtime is enforced at admission**: session pods, Sandboxes and templates
  without `gvisor` or `kata` are rejected ([../policies](../policies/README.md)).
