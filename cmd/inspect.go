package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/config"
	"github.com/nuonco/nuon/pkg/runner/airgap"
	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

func inspectCmd() *cobra.Command {
	var jsonOutput bool
	var extractPlan string
	var extractRunner string
	c := &cobra.Command{Use: "inspect <bundle.tar.zst>", Short: "show a bundle's contents", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		o, err := openArchive(cmd, args[0])
		if err != nil {
			return err
		}
		defer o.cleanup()
		if extractPlan != "" {
			if len(o.bundle.PlanEnvelope) == 0 {
				return fmt.Errorf("bundle has no plan envelope")
			}
			if err := os.WriteFile(extractPlan, append(bytes.TrimSpace(o.bundle.PlanEnvelope), '\n'), 0o600); err != nil {
				return fmt.Errorf("write plan envelope: %w", err)
			}
			fmt.Fprintf(os.Stderr, "wrote plan envelope to %s\n", extractPlan)
		}
		if extractRunner != "" {
			if err := writeRunnerBinary(cmd, o.bundle, extractRunner); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote runner binary to %s\n", extractRunner)
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(struct {
				TransportChecksum string          `json:"transport_checksum"`
				Size              int64           `json:"size"`
				Manifest          any             `json:"manifest"`
				Members           any             `json:"members"`
				PlanEnvelope      json.RawMessage `json:"plan_envelope,omitempty"`
			}{o.checksum, o.size, o.bundle.Manifest, o.bundle.Members(), o.bundle.PlanEnvelope})
		}
		fmt.Printf("checksum sha256:%s  size %s\nbundle target %s/%s  schema %d\n", o.checksum, humanSize(o.size), o.bundle.Manifest.Target.OS, o.bundle.Manifest.Target.Architecture, o.bundle.Manifest.SchemaVersion)
		printMembers(o.bundle.Members())
		printPlanEnvelope(o.bundle.PlanEnvelope)
		return nil
	}}
	c.Flags().BoolVar(&jsonOutput, "json", false, "print JSON")
	c.Flags().StringVar(&extractPlan, "extract-plan", "", "write the plan envelope JSON to the given path")
	c.Flags().StringVar(&extractRunner, "extract-runner", "", "write the packaged runner binary to the given path")
	return c
}

func writeRunnerBinary(cmd *cobra.Command, b *bundle.Bundle, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create runner binary: %w", err)
	}
	if err := b.ExtractRunnerBinary(cmd.Context(), f); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

func printPlanEnvelope(raw json.RawMessage) {
	fmt.Printf("\nPLAN\n")
	if len(raw) == 0 {
		fmt.Println("(none)")
		return
	}
	envelope, err := airgap.Parse(raw)
	if err != nil {
		fmt.Printf("(unreadable: %v)\n", err)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STEP\tTYPE\tOPERATION\tGROUP\tDEPENDS ON\tPLAN FROM")
	for _, s := range envelope.Steps {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.JobType, s.JobOperation, s.JobGroup, strings.Join(s.DependsOn, ","), s.PlanFromStep)
	}
	_ = w.Flush()
	fmt.Printf("\nINPUTS\n")
	if len(envelope.Inputs) == 0 {
		fmt.Println("(none)")
		return
	}
	w = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tREQUIRED\tDEFAULT\tOFFLINE")
	for _, in := range envelope.Inputs {
		offline := "fixed at publish"
		if in.Secret {
			offline = "secret"
		} else if in.Bindable {
			offline = "editable"
		}
		fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\n", displayInputName(in.Name), in.Type, in.Required, in.Default, offline)
	}
	_ = w.Flush()
}

func displayInputName(name string) string {
	kind, component, ok := config.ParseComponentOverrideInputName(name)
	if !ok {
		return name
	}
	return fmt.Sprintf("override:%s:%s", kind, component)
}

func printMembers(ms []bundle.Member) {
	sections := []struct{ kind, title string }{{"component", "COMPONENTS"}, {"sandbox", "SANDBOX"}, {"image", "IMAGES"}, {"action-artifact", "ACTION ARTIFACTS"}, {"stack-asset", "STACK ASSETS"}, {"runner-binary", "RUNNER BINARY"}, {"runner-image", "RUNNER IMAGE"}}
	for _, section := range sections {
		fmt.Printf("\n%s\n", section.title)
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tMEDIA TYPE\tSIZE\tDIGEST")
		for _, m := range ms {
			if m.Kind == section.kind {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.Name, m.MediaType, humanSize(m.Size), shortDigest(m.Digest.String()))
			}
		}
		_ = w.Flush()
	}
}
