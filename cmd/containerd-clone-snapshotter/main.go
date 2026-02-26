//go:build linux

// containerd-clone-snapshotter is a containerd proxy-snapshotter plugin that
// adds container-cloning capability on top of the Linux overlayfs snapshotter.
//
// When containerd prepares a snapshot with the label
// "containerd.io/snapshot/clone-source=<key>", the plugin creates a new
// snapshot whose writable layer is initialised with a copy of the source
// snapshot's writable layer.  This lets you start a new container that begins
// with the exact same filesystem state as an existing container.
//
// # Configuration
//
// Add the following stanza to /etc/containerd/config.toml to register the
// plugin:
//
//	[proxy_plugins]
//	  [proxy_plugins.clone]
//	    type    = "snapshot"
//	    address = "/run/containerd-clone-snapshotter/containerd-clone-snapshotter.sock"
//
// Then set the snapshotter when creating containers:
//
//	ctr run --snapshotter clone <image> <container-id>
//
// # Usage
//
//	containerd-clone-snapshotter [flags]
//
//	Flags:
//	  -socket  string  Unix socket path (default: /run/containerd-clone-snapshotter/containerd-clone-snapshotter.sock)
//	  -root    string  Root directory for snapshot storage (default: /var/lib/containerd-clone-snapshotter)
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/contrib/snapshotservice"
	"github.com/containerd/containerd/snapshots/overlay"
	"github.com/fengqi-dev/containerd-clone-snapshotter/snapshotter"
	"google.golang.org/grpc"
)

func main() {
	socketPath := flag.String(
		"socket",
		"/run/containerd-clone-snapshotter/containerd-clone-snapshotter.sock",
		"Unix socket path that containerd connects to",
	)
	rootDir := flag.String(
		"root",
		"/var/lib/containerd-clone-snapshotter",
		"Root directory used to store snapshot data",
	)
	flag.Parse()

	// Ensure the socket directory exists.
	if err := os.MkdirAll(filepath.Dir(*socketPath), 0700); err != nil {
		log.Fatalf("create socket directory: %v", err)
	}

	// Remove a stale socket from a previous run.
	if err := os.Remove(*socketPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("remove stale socket: %v", err)
	}

	// Create the root storage directory.
	if err := os.MkdirAll(*rootDir, 0700); err != nil {
		log.Fatalf("create root directory: %v", err)
	}

	// Initialise the underlying overlayfs snapshotter.
	inner, err := overlay.NewSnapshotter(*rootDir)
	if err != nil {
		log.Fatalf("create overlayfs snapshotter: %v", err)
	}

	// Wrap it with the clone-aware snapshotter.
	sn := snapshotter.New(inner)

	// Build the gRPC snapshots service from the snapshotter.
	service := snapshotservice.FromSnapshotter(sn)

	// Listen on the Unix socket.
	listener, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("listen on %q: %v", *socketPath, err)
	}

	// Register the service and start serving.
	grpcServer := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(grpcServer, service)

	// Graceful shutdown on SIGINT / SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received signal %v, shutting down", sig)
		grpcServer.GracefulStop()
	}()

	log.Printf("containerd-clone-snapshotter listening on %s (root: %s)", *socketPath, *rootDir)
	if err := grpcServer.Serve(listener); err != nil {
		log.Printf("gRPC server stopped: %v", err)
	}
}
