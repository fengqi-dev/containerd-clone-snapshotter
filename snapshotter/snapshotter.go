// Package snapshotter implements a containerd snapshotter plugin that supports
// cloning an existing container's filesystem when creating a new container.
//
// When Prepare is called with the [LabelCloneSource] label pointing to an
// existing active snapshot, the new snapshot is initialized with a copy of
// the source snapshot's writable layer, giving the new container the same
// filesystem state as the source container.
package snapshotter

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
)

// LabelCloneSource is the snapshot label key used to specify the source
// snapshot whose writable layer should be cloned into the new snapshot.
//
// Set this label in the opts passed to Prepare to trigger the clone behaviour:
//
//	snapshotter.Prepare(ctx, "new-container", "",
//	    snapshots.WithLabels(map[string]string{
//	        LabelCloneSource: "source-container",
//	    }),
//	)
const LabelCloneSource = "containerd.io/snapshot/clone-source"

// CloneSnapshotter wraps any snapshots.Snapshotter and adds container-cloning
// capability. All methods are delegated to the inner snapshotter; the only
// exception is Prepare, which intercepts requests that carry [LabelCloneSource].
type CloneSnapshotter struct {
	snapshots.Snapshotter
}

// New returns a CloneSnapshotter that wraps inner.
func New(inner snapshots.Snapshotter) *CloneSnapshotter {
	return &CloneSnapshotter{Snapshotter: inner}
}

// Prepare creates an active snapshot identified by key.
//
// If the [LabelCloneSource] label is present in opts, Prepare clones the
// source snapshot instead of using parent:
//  1. The source snapshot's info is retrieved to find its parent.
//  2. A new snapshot is prepared from that same parent.
//  3. The source's writable layer is copied into the new snapshot.
//
// The [LabelCloneSource] label is stripped before the inner Prepare call to
// avoid infinite recursion and to keep the stored snapshot metadata clean.
func (s *CloneSnapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	info := snapshots.Info{}
	for _, opt := range opts {
		if err := opt(&info); err != nil {
			return nil, err
		}
	}

	sourceKey, ok := info.Labels[LabelCloneSource]
	if !ok {
		return s.Snapshotter.Prepare(ctx, key, parent, opts...)
	}

	return s.clonePrepare(ctx, key, sourceKey, opts)
}

// clonePrepare implements the clone logic: it prepares a new snapshot with
// the same parent as the source and then copies the source's writable layer.
func (s *CloneSnapshotter) clonePrepare(ctx context.Context, key, sourceKey string, opts []snapshots.Opt) ([]mount.Mount, error) {
	// Retrieve source info to learn its parent snapshot chain.
	sourceInfo, err := s.Snapshotter.Stat(ctx, sourceKey)
	if err != nil {
		return nil, fmt.Errorf("stat source snapshot %q: %w", sourceKey, err)
	}

	// Get source mounts to locate the writable directory we need to copy.
	sourceMounts, err := s.Snapshotter.Mounts(ctx, sourceKey)
	if err != nil {
		return nil, fmt.Errorf("get mounts for source snapshot %q: %w", sourceKey, err)
	}

	// Prepare the new snapshot with the same parent as the source.
	// The clone label is stripped to prevent infinite recursion and to avoid
	// storing it on the new snapshot's metadata.
	innerOpts := withoutLabel(opts, LabelCloneSource)
	mounts, err := s.Snapshotter.Prepare(ctx, key, sourceInfo.Parent, innerOpts...)
	if err != nil {
		return nil, fmt.Errorf("prepare snapshot %q: %w", key, err)
	}

	// Copy the writable layer from source to the new snapshot.
	if err := copyWritableLayer(sourceMounts, mounts); err != nil {
		if removeErr := s.Snapshotter.Remove(ctx, key); removeErr != nil {
			return nil, fmt.Errorf("copy writable layer: %w (cleanup also failed: %v)", err, removeErr)
		}
		return nil, fmt.Errorf("copy writable layer from %q to %q: %w", sourceKey, key, err)
	}

	return mounts, nil
}

// withoutLabel returns a single opts function that applies all of the original
// opts and then deletes the named label, preventing it from being stored on
// the new snapshot.
func withoutLabel(opts []snapshots.Opt, label string) []snapshots.Opt {
	return []snapshots.Opt{func(info *snapshots.Info) error {
		for _, opt := range opts {
			if err := opt(info); err != nil {
				return err
			}
		}
		delete(info.Labels, label)
		return nil
	}}
}

// copyWritableLayer copies the contents of the source snapshot's writable
// directory into the destination snapshot's writable directory.
//
// For overlay mounts the writable directory is the upperdir= option value.
// For bind mounts (used by the native snapshotter) it is the mount source.
// The destination directory is cleared first so that files deleted in the
// source are not preserved in the clone.
func copyWritableLayer(srcMounts, dstMounts []mount.Mount) error {
	srcDir, err := getWritableDir(srcMounts)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	dstDir, err := getWritableDir(dstMounts)
	if err != nil {
		return fmt.Errorf("destination: %w", err)
	}

	// Clear destination first so files deleted in the source are not kept.
	if err := clearDir(dstDir); err != nil {
		return fmt.Errorf("clear destination directory: %w", err)
	}

	return copyDir(srcDir, dstDir)
}

// getWritableDir extracts the writable directory path from a set of mounts.
//   - overlay: returns the upperdir= option value
//   - bind:    returns the mount source path
func getWritableDir(mounts []mount.Mount) (string, error) {
	for _, m := range mounts {
		switch m.Type {
		case "overlay":
			for _, opt := range m.Options {
				if val, ok := strings.CutPrefix(opt, "upperdir="); ok {
					return val, nil
				}
			}
		case "bind":
			return m.Source, nil
		}
	}
	return "", fmt.Errorf("no writable directory found in mounts (types: %s)", joinMountTypes(mounts))
}

// joinMountTypes returns a comma-separated list of mount types for diagnostics.
func joinMountTypes(mounts []mount.Mount) string {
	types := make([]string, len(mounts))
	for i, m := range mounts {
		types[i] = m.Type
	}
	return strings.Join(types, ", ")
}

// clearDir removes all entries inside dir without removing dir itself.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// copyDir recursively copies the contents of srcDir into dstDir, preserving
// permissions. Symlinks are recreated as symlinks; directories and regular
// files are copied with their mode bits.
func copyDir(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		// Skip the root entry; dstDir already exists.
		if rel == "." {
			return nil
		}

		dst := filepath.Join(dstDir, rel)

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return copySymlink(path, dst)

		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(dst, info.Mode().Perm())

		default:
			info, err := d.Info()
			if err != nil {
				return err
			}
			return copyFile(path, dst, info.Mode().Perm())
		}
	})
}

// copySymlink creates a symlink at dst pointing to the same target as src.
func copySymlink(src, dst string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	return os.Symlink(target, dst)
}

// copyFile copies a regular file from src to dst using the provided mode bits.
func copyFile(src, dst string, mode os.FileMode) (retErr error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && retErr == nil {
			retErr = cerr
		}
	}()

	_, err = io.Copy(out, in)
	return err
}
