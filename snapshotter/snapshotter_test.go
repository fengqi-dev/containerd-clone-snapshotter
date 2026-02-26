package snapshotter_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/native"
	"github.com/fengqi-dev/containerd-clone-snapshotter/snapshotter"
)

// newTestSnapshotter creates a CloneSnapshotter backed by the native
// snapshotter rooted at a temporary directory.  It returns the snapshotter
// and a cleanup function.
func newTestSnapshotter(t *testing.T) (*snapshotter.CloneSnapshotter, func()) {
	t.Helper()
	root := t.TempDir()
	inner, err := native.NewSnapshotter(root)
	if err != nil {
		t.Fatalf("create native snapshotter: %v", err)
	}
	return snapshotter.New(inner), func() {}
}

// writableDir extracts the bind-mount source path from the mounts returned
// by the native snapshotter so tests can read/write files directly.
func writableDir(t *testing.T, sn *snapshotter.CloneSnapshotter, key string) string {
	t.Helper()
	ctx := context.Background()
	mounts, err := sn.Mounts(ctx, key)
	if err != nil {
		t.Fatalf("Mounts(%q): %v", key, err)
	}
	for _, m := range mounts {
		if m.Type == "bind" {
			return m.Source
		}
	}
	t.Fatalf("no bind mount found for snapshot %q", key)
	return ""
}

// TestPrepare_NormalDelegation verifies that Prepare without the clone label
// is forwarded to the inner snapshotter unchanged, and that the committed
// snapshot is accessible in subsequent operations.
func TestPrepare_NormalDelegation(t *testing.T) {
	ctx := context.Background()
	sn, cleanup := newTestSnapshotter(t)
	defer cleanup()

	mounts, err := sn.Prepare(ctx, "layer1", "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(mounts) == 0 {
		t.Fatal("expected at least one mount")
	}

	dir := writableDir(t, sn, "layer1")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := sn.Commit(ctx, "layer1-committed", "layer1"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify the committed snapshot is accessible via Stat.
	info, err := sn.Stat(ctx, "layer1-committed")
	if err != nil {
		t.Fatalf("Stat committed snapshot: %v", err)
	}
	if info.Kind != snapshots.KindCommitted {
		t.Errorf("snapshot kind = %v, want KindCommitted", info.Kind)
	}

	// Verify the committed snapshot can be used as a parent for a new layer.
	if _, err := sn.Prepare(ctx, "layer2", "layer1-committed"); err != nil {
		t.Fatalf("Prepare child snapshot: %v", err)
	}
	assertFileContent(t, writableDir(t, sn, "layer2"), "file.txt", "hello")
}

// TestPrepare_Clone verifies that Prepare with the clone label creates a new
// snapshot whose writable layer is a copy of the source snapshot.
func TestPrepare_Clone(t *testing.T) {
	ctx := context.Background()
	sn, cleanup := newTestSnapshotter(t)
	defer cleanup()

	// Build a committed base layer.
	if _, err := sn.Prepare(ctx, "base", ""); err != nil {
		t.Fatalf("Prepare base: %v", err)
	}
	baseDir := writableDir(t, sn, "base")
	if err := os.WriteFile(filepath.Join(baseDir, "base.txt"), []byte("base"), 0644); err != nil {
		t.Fatalf("write base file: %v", err)
	}
	if err := sn.Commit(ctx, "base-committed", "base"); err != nil {
		t.Fatalf("Commit base: %v", err)
	}

	// Create the source "container" snapshot on top of the base layer.
	if _, err := sn.Prepare(ctx, "source-container", "base-committed"); err != nil {
		t.Fatalf("Prepare source: %v", err)
	}
	srcDir := writableDir(t, sn, "source-container")
	if err := os.WriteFile(filepath.Join(srcDir, "container.txt"), []byte("container-data"), 0644); err != nil {
		t.Fatalf("write container file: %v", err)
	}

	// Create a clone of the source container.
	_, err := sn.Prepare(ctx, "cloned-container", "",
		snapshots.WithLabels(map[string]string{
			snapshotter.LabelCloneSource: "source-container",
		}),
	)
	if err != nil {
		t.Fatalf("Prepare clone: %v", err)
	}

	cloneDir := writableDir(t, sn, "cloned-container")

	// The clone must contain the base file (inherited via the parent layer).
	assertFileContent(t, cloneDir, "base.txt", "base")
	// The clone must contain the source container's writable-layer file.
	assertFileContent(t, cloneDir, "container.txt", "container-data")
}

// TestPrepare_Clone_DeletedFiles verifies that files deleted in the source
// container are also absent in the clone (not preserved from the parent layer).
func TestPrepare_Clone_DeletedFiles(t *testing.T) {
	ctx := context.Background()
	sn, cleanup := newTestSnapshotter(t)
	defer cleanup()

	// Build a committed base layer that contains a file.
	if _, err := sn.Prepare(ctx, "base2", ""); err != nil {
		t.Fatalf("Prepare base2: %v", err)
	}
	baseDir := writableDir(t, sn, "base2")
	if err := os.WriteFile(filepath.Join(baseDir, "will-be-deleted.txt"), []byte("delete me"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := sn.Commit(ctx, "base2-committed", "base2"); err != nil {
		t.Fatalf("Commit base2: %v", err)
	}

	// Create source container and delete the file.
	if _, err := sn.Prepare(ctx, "source2", "base2-committed"); err != nil {
		t.Fatalf("Prepare source2: %v", err)
	}
	srcDir := writableDir(t, sn, "source2")
	if err := os.Remove(filepath.Join(srcDir, "will-be-deleted.txt")); err != nil {
		t.Fatalf("remove file: %v", err)
	}

	// Clone the source.
	if _, err := sn.Prepare(ctx, "clone2", "",
		snapshots.WithLabels(map[string]string{
			snapshotter.LabelCloneSource: "source2",
		}),
	); err != nil {
		t.Fatalf("Prepare clone2: %v", err)
	}

	cloneDir := writableDir(t, sn, "clone2")

	// The deleted file must NOT be present in the clone.
	if _, err := os.Stat(filepath.Join(cloneDir, "will-be-deleted.txt")); !os.IsNotExist(err) {
		t.Error("expected will-be-deleted.txt to be absent in clone, but it exists")
	}
}

// TestPrepare_Clone_MissingSource verifies that Prepare returns an error when
// the source snapshot does not exist.
func TestPrepare_Clone_MissingSource(t *testing.T) {
	ctx := context.Background()
	sn, cleanup := newTestSnapshotter(t)
	defer cleanup()

	_, err := sn.Prepare(ctx, "clone-bad", "",
		snapshots.WithLabels(map[string]string{
			snapshotter.LabelCloneSource: "does-not-exist",
		}),
	)
	if err == nil {
		t.Fatal("expected error for missing source snapshot, got nil")
	}
}

// TestPrepare_Clone_Symlink verifies that symlinks in the source snapshot are
// properly recreated in the clone.
func TestPrepare_Clone_Symlink(t *testing.T) {
	ctx := context.Background()
	sn, cleanup := newTestSnapshotter(t)
	defer cleanup()

	if _, err := sn.Prepare(ctx, "sym-src", ""); err != nil {
		t.Fatalf("Prepare sym-src: %v", err)
	}
	srcDir := writableDir(t, sn, "sym-src")
	if err := os.WriteFile(filepath.Join(srcDir, "real.txt"), []byte("real"), 0644); err != nil {
		t.Fatalf("write real.txt: %v", err)
	}
	if err := os.Symlink("real.txt", filepath.Join(srcDir, "link.txt")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if _, err := sn.Prepare(ctx, "sym-clone", "",
		snapshots.WithLabels(map[string]string{
			snapshotter.LabelCloneSource: "sym-src",
		}),
	); err != nil {
		t.Fatalf("Prepare sym-clone: %v", err)
	}

	cloneDir := writableDir(t, sn, "sym-clone")

	target, err := os.Readlink(filepath.Join(cloneDir, "link.txt"))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != "real.txt" {
		t.Errorf("symlink target = %q, want %q", target, "real.txt")
	}
	assertFileContent(t, cloneDir, "real.txt", "real")
}

// assertFileContent reads the file at dir/name and checks its content.
func assertFileContent(t *testing.T, dir, name, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s/%s: %v", dir, name, err)
	}
	if got := string(data); got != want {
		t.Errorf("%s: content = %q, want %q", name, got, want)
	}
}
