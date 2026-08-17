package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/runner/airgap"
	"github.com/nuonco/nuon/pkg/runner/airgap/statestore"
)

func resultsCmd() *cobra.Command {
	var state, profile, region string
	var jsonOutput bool
	var out string
	c := &cobra.Command{Use: "results [--state <dir|s3://bucket/prefix>]", Short: "show the post-trip report of an offline run", RunE: func(cmd *cobra.Command, _ []string) error {
		if err := resolveStateFlags(&state, &profile, &region); err != nil {
			return err
		}
		fetch, err := makeStateFetcher(cmd.Context(), state, profile, region)
		if err != nil {
			return err
		}
		raw, ok, err := fetch(cmd.Context(), "report.json")
		if err != nil {
			return fmt.Errorf("read report: %w", err)
		}
		if !ok {
			statusRaw, statusOK, err := fetch(cmd.Context(), "status.json")
			if err != nil {
				return fmt.Errorf("read status: %w", err)
			}
			if !statusOK {
				return fmt.Errorf("state has no run: %s", state)
			}
			var status statestore.Status
			if err := json.Unmarshal(statusRaw, &status); err != nil {
				return fmt.Errorf("decode status.json: %w", err)
			}
			return fmt.Errorf("run has not finished (status %s); re-run `runner airgap` to completion or check status.json", status.Status)
		}
		if out != "" {
			if err := os.WriteFile(out, raw, 0o600); err != nil {
				return fmt.Errorf("write report: %w", err)
			}
			fmt.Fprintf(os.Stderr, "wrote report to %s\n", out)
		}
		if jsonOutput {
			_, err := os.Stdout.Write(raw)
			return err
		}
		var report airgap.Report
		if err := json.Unmarshal(raw, &report); err != nil {
			return fmt.Errorf("decode report: %w", err)
		}
		printReport(&report)
		return nil
	}}
	c.Flags().StringVar(&state, "state", "", "runner state directory or s3://bucket/prefix (defaults to the active context)")
	c.Flags().StringVar(&profile, "profile", "", "AWS profile for S3 state (defaults to the active context)")
	c.Flags().StringVar(&region, "region", "", "AWS region for S3 state (defaults to the active context)")
	c.Flags().BoolVar(&jsonOutput, "json", false, "print JSON")
	c.Flags().StringVar(&out, "out", "", "copy report.json to the given path")
	return c
}

func printReport(report *airgap.Report) {
	fmt.Printf("install %s  run %s\nstatus %s", report.InstallID, report.RunID, report.Status)
	if report.FailedStep != "" {
		fmt.Printf("  failed step %s", report.FailedStep)
	}
	fmt.Printf("\nstarted %s", report.StartedAt.Format("2006-01-02 15:04:05 MST"))
	if report.FinishedAt != nil {
		fmt.Printf("  finished %s  took %s", report.FinishedAt.Format("2006-01-02 15:04:05 MST"), report.FinishedAt.Sub(report.StartedAt).Round(1e9))
	}
	fmt.Printf("\n\nSTEPS\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STEP\tTYPE\tSTATUS\tSUCCESS\tEXECUTIONS\tOUTPUTS\tERROR")
	for _, s := range report.Steps {
		success := ""
		if s.Success != nil {
			success = fmt.Sprintf("%t", *s.Success)
		}
		outputs := ""
		if len(s.Outputs) > 0 {
			outputs = "yes"
		}
		errText := s.Error
		if errText == "" {
			errText = s.ErrorCode
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", s.ID, s.JobType, s.Status, success, s.Executions, outputs, errText)
	}
	_ = w.Flush()
}
