package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

func verifyCmd() *cobra.Command {
	return &cobra.Command{Use: "verify <bundle.tar.zst>", Short: "verify a bundle archive's integrity", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		o, err := openArchive(cmd, args[0])
		if err != nil {
			return err
		}
		defer o.cleanup()
		fmt.Println("transport checksum: ok")
		fmt.Println("bundle manifest: ok")
		if err := bundle.VerifyBlobs(o.dir); err != nil {
			return err
		}
		fmt.Println("blob digests: ok")
		blobs, _ := filepath.Glob(filepath.Join(o.dir, "blobs", "sha256", "*"))
		fmt.Printf("verified %d artifacts, %d blobs, %s\n", len(o.bundle.Members()), len(blobs), humanSize(o.size))
		return nil
	}}
}
