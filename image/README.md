# Sandbox Image

For this demo, images are built locally and pushed to a local registry.

## Demo: Ensure a local registry is started:

/etc/docker/daemon.json:

```json
{
  "insecure-registries": ["localhost:5000"]
}
```

Start a local registry:

```shell
docker run -d -p 5000:5000 --restart always --name registry registry:3
```

## Images

| Image | Tag | Purpose |
|---|---|---|
| [`devbox/`](devbox/README.md) | `localhost:5000/devbox:2` | **All agent sandboxes** (`k8s/developers` and `agent-sessions`): Claude Code, herdr-core, rtk; reached over k8s exec only. Everything pinned |
| `testing/` | `localhost:5000/test:1` | Minimal smoke-test image (CRD install, registry, runtimes). No longer used by any template |

Build and publish the test image:

```shell
cd testing
sudo docker buildx build . -t localhost:5000/test:1
sudo docker image push localhost:5000/test:1
```

For devbox, see [devbox/README.md](devbox/README.md).
