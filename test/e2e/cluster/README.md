# E2E Cluster Setup

`start-cluster.sh` creates (or reuses) a local k3d cluster for end-to-end tests.

## Why inotify limits matter

k3s uses Linux inotify watchers internally via containerd for image import tracking.
The host kernel parameter `fs.inotify.max_user_instances` caps how many inotify
instances any single user can hold. Its default value is **128**.

Each k3s node (server or agent) opens several inotify instances. A single k3d cluster
with 1 server + 3 agents can consume 40–80 instances. Running **two** clusters
concurrently (e.g. this project and `gitops-reverser` open side by side in the same
devcontainer) can easily exhaust the limit, causing k3s to crash on startup with:

```
failed to create image import watcher: too many open files
```

Because this devcontainer uses **Docker-outside-of-Docker** (DooD), all containers —
including k3s nodes — share the same host kernel. So the `max_user_instances` limit
is shared across all running clusters.

### The fix

`start-cluster.sh` calls `ensure_inotify_limits` before creating the cluster. It
reads the current limit and, if it is below 512, runs a short-lived privileged
Alpine container to raise it:

```bash
docker run --rm --privileged alpine \
  sysctl -w fs.inotify.max_user_instances=512
```

A privileged container **can write host kernel parameters** — the sysctl write
propagates to the real kernel, not just the container's namespace. This is why the
fix works even though we are inside a devcontainer.

The value 512 comfortably covers two concurrent four-node clusters with headroom to
spare. It reverts to the system default when the host machine reboots.

## Running two clusters concurrently

If you have `gitops-reverser` open in the same devcontainer session, both clusters
share the same Docker daemon. The inotify preflight handles this automatically, but
be aware that:

- Both clusters consume memory on the Docker host.
- Port ranges for k3d API ports (6550–6554) are shared; `start-cluster.sh` picks the
  first free port automatically.
- `kubectl` context switching is needed when working across both projects.


Some background:
https://maestral.app/docs/inotify-limits

Checking the current limits: `sysctl fs.inotify`
