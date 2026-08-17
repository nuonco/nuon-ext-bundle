package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

func TestOpenArchiveUsesPrivateChildWorkdir(t *testing.T) {
	parent := t.TempDir()
	existing := filepath.Join(parent, "blobs")
	require.NoError(t, os.Symlink(t.TempDir(), existing))
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.zst")
	var archive bytes.Buffer
	_, err := bundle.Generate(context.Background(), &archive, bundle.LogicalManifest{
		SchemaVersion: 1,
		Target:        bundle.Target{OS: "linux", Architecture: "amd64"},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(archivePath, archive.Bytes(), 0644))

	previousWorkdir, previousKeep := workdir, keepWorkdir
	t.Cleanup(func() { workdir, keepWorkdir = previousWorkdir, previousKeep })
	workdir, keepWorkdir = parent, true
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	o, err := openArchive(cmd, archivePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(o.dir) })
	require.Equal(t, parent, filepath.Dir(o.dir))
	require.NotEqual(t, parent, o.dir)
	require.DirExists(t, o.dir)
	info, err := os.Lstat(existing)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
}
