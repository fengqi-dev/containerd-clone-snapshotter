# containerd-clone-snapshotter

A containerd [proxy-snapshotter](https://github.com/containerd/containerd/blob/main/docs/snapshotters/remote-snapshotter.md) plugin that allows a new container to be created as a **clone of an existing container's filesystem state**.

## How it works

containerd manages container filesystems through _snapshots_.  Each container
has an active (writable) snapshot whose lower layers are the image layers and
whose upper (writable) layer accumulates all changes made inside the container.

`containerd-clone-snapshotter` wraps the Linux **overlayfs** snapshotter.  When
`Prepare` is called with the special label
`containerd.io/snapshot/clone-source=<key>`, the plugin:

1. Looks up the source snapshot to find its parent chain.
2. Creates a new snapshot from that same parent.
3. Copies the source snapshot's writable layer into the new snapshot's writable
   layer.

The result is a new container that starts with an identical copy of the source
container's filesystem — including all files written, modified, or deleted
inside the running source container.

```
Image layers (read-only, shared)
        │
        ▼
  base-committed  ──┬──────────────────────────┐
                    │                          │
               source-container           cloned-container
               (upper: modified)          (upper: copy of source upper)
```

## Installation

### Build

```sh
go build -o containerd-clone-snapshotter ./cmd/containerd-clone-snapshotter
```

### Run

```sh
sudo containerd-clone-snapshotter \
    -socket /run/containerd-clone-snapshotter/containerd-clone-snapshotter.sock \
    -root   /var/lib/containerd-clone-snapshotter
```

### Configure containerd

Add the following to `/etc/containerd/config.toml` and restart containerd:

```toml
[proxy_plugins]
  [proxy_plugins.clone]
    type    = "snapshot"
    address = "/run/containerd-clone-snapshotter/containerd-clone-snapshotter.sock"
```

## Usage

### Pull an image and start a source container

```sh
ctr image pull docker.io/library/alpine:latest

ctr run --snapshotter clone \
    docker.io/library/alpine:latest \
    source-container \
    sh -c "echo 'hello from source' > /data.txt && sleep 3600"
```

### Create a clone of the running container

Use the `containerd.io/snapshot/clone-source` label when creating the new
container's snapshot, pointing it at the **active snapshot key** of the source
container (which is typically the container ID in `ctr`):

```sh
ctr snapshots --snapshotter clone prepare \
    --label containerd.io/snapshot/clone-source=source-container \
    cloned-container ""

ctr run --snapshotter clone \
    --with-ns "network:/var/run/netns/default" \
    docker.io/library/alpine:latest \
    cloned-container \
    sh -c "cat /data.txt"   # prints: hello from source
```

## Architecture

```
containerd
    │  gRPC (snapshots API)
    ▼
containerd-clone-snapshotter  ← this project
    │  wraps
    ▼
overlayfs snapshotter  (github.com/containerd/containerd/snapshots/overlay)
```

The `snapshotter.CloneSnapshotter` type in package `snapshotter` is a thin
wrapper around **any** `snapshots.Snapshotter`.  It intercepts only `Prepare`
calls that carry the clone label; all other calls are forwarded unchanged.

## Development

```sh
go test ./...
go build ./...
```

## Label reference

| Label | Value | Effect |
|-------|-------|--------|
| `containerd.io/snapshot/clone-source` | snapshot key | Clone the named snapshot's writable layer into the new snapshot |