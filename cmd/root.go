package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

var workdir string
var keepWorkdir bool

func Execute() {
	root := &cobra.Command{Use: "nuon-bundle", Short: "Work with Nuon air-gap bundles offline", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&workdir, "workdir", "", "directory used to extract the bundle")
	root.PersistentFlags().BoolVar(&keepWorkdir, "keep-workdir", false, "keep the extracted bundle")
	root.AddCommand(initCmd(), inspectCmd(), verifyCmd(), pushCmd(), resultsCmd(), stackCmd(), deployCmd(), statusCmd(), healthCmd(), logsCmd(), ctxCmd(), refsCmd(), runCmd(), runsCmd(), portalCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type opened struct {
	bundle        *bundle.Bundle
	checksum, dir string
	size          int64
	cleanup       func()
}

func openArchive(cmd *cobra.Command, path string) (*opened, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("unable to open bundle: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("unable to stat bundle: %w", err)
	}
	if workdir != "" {
		if err := os.MkdirAll(workdir, 0755); err != nil {
			return nil, fmt.Errorf("unable to create workdir: %w", err)
		}
	}
	dir, err := os.MkdirTemp(workdir, "nuon-bundle-*")
	if err != nil {
		return nil, fmt.Errorf("unable to create workdir: %w", err)
	}
	cleanup := func() {}
	removeDir := func() { _ = os.RemoveAll(dir) }
	if !keepWorkdir {
		cleanup = removeDir
	}
	checksum, err := bundle.Extract(dir, f)
	if err != nil {
		removeDir()
		return nil, err
	}
	b, err := bundle.Open(cmd.Context(), dir)
	if err != nil {
		removeDir()
		return nil, err
	}
	return &opened{bundle: b, checksum: checksum, dir: dir, size: info.Size(), cleanup: cleanup}, nil
}

func shortDigest(s string) string {
	if len(s) > 19 {
		return s[:19] + "…"
	}
	return s
}
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
func receiptPath(path string) string {
	return filepath.Join(filepath.Dir(path), filepath.Base(path)+".pushed.json")
}
