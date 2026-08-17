package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/runner/airgap/day2"
)

func runsCmd() *cobra.Command {
	var state, profile, region string
	var jsonOutput bool
	c := &cobra.Command{Use: "runs [run-id]", Short: "list or inspect day-2 runs", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := resolveStateFlags(&state, &profile, &region); err != nil {
			return err
		}
		store, err := makeDay2State(cmd.Context(), state, profile, region)
		if err != nil {
			return err
		}
		if len(args) == 1 {
			run, err := readRun(cmd.Context(), store, args[0])
			if err != nil {
				return err
			}
			if run == nil {
				return fmt.Errorf("run %q not found", args[0])
			}
			return printRun(cmd.OutOrStdout(), run, jsonOutput, state)
		}
		runs, err := listRuns(cmd.Context(), store)
		if err != nil {
			return err
		}
		return printRuns(cmd.OutOrStdout(), runs, jsonOutput)
	}}
	stateFlags(c, &state, &profile, &region)
	c.Flags().BoolVar(&jsonOutput, "json", false, "print JSON")
	return c
}

func readRun(ctx context.Context, store day2State, id string) (*day2.RunStatus, error) {
	raw, ok, err := store.Get(ctx, day2.RunStatusKey(id))
	if err != nil || !ok {
		return nil, err
	}
	var run day2.RunStatus
	if err := json.Unmarshal(raw, &run); err != nil {
		return nil, fmt.Errorf("decode run %s: %w", id, err)
	}
	return &run, nil
}

func listRuns(ctx context.Context, store day2State) ([]day2.RunStatus, error) {
	keys, err := store.List(ctx, day2.RunsPrefix)
	if err != nil {
		return nil, err
	}
	runs := make([]day2.RunStatus, 0)
	for _, key := range keys {
		if !strings.HasSuffix(key, "/status.json") {
			continue
		}
		raw, ok, err := store.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var run day2.RunStatus
		if err := json.Unmarshal(raw, &run); err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

func printRuns(w io.Writer, runs []day2.RunStatus, jsonOutput bool) error {
	if jsonOutput {
		raw, err := json.MarshalIndent(runs, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(raw))
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN ID\tREF\tKIND\tSOURCE\tSTATUS\tSTARTED\tDURATION")
	for _, run := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", run.RunID, run.RefID, run.RefKind, run.Source, run.Status, run.StartedAt.Local().Format(time.RFC3339), runDuration(run))
	}
	return tw.Flush()
}

func printRun(w io.Writer, run *day2.RunStatus, jsonOutput bool, state string) error {
	if jsonOutput {
		raw, err := json.MarshalIndent(run, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(raw))
		return err
	}
	fmt.Fprintf(w, "run %s  ref %s (%s)  source %s  status %s\nstarted %s  duration %s\n", run.RunID, run.RefID, run.RefKind, run.Source, run.Status, run.StartedAt.Local().Format(time.RFC3339), runDuration(*run))
	if run.Error != "" {
		fmt.Fprintf(w, "error: %s\n", run.Error)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tKIND\tSTATUS\tJOB ID\tERROR")
	for _, step := range run.Steps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", step.Name, step.Kind, step.Status, step.JobID, step.Error)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	printDrift(w, run)
	for _, step := range run.Steps {
		if step.JobID != "" {
			fmt.Fprintf(w, "\nlogs: %s\n", logsCommandHint(step.JobID, state))
		}
	}
	return nil
}

func runDuration(run day2.RunStatus) string {
	if run.StartedAt.IsZero() {
		return ""
	}
	end := time.Now()
	if run.FinishedAt != nil {
		end = *run.FinishedAt
	}
	return end.Sub(run.StartedAt).Round(time.Second).String()
}

func printDrift(w io.Writer, run *day2.RunStatus) {
	for _, step := range run.Steps {
		if step.Drift == nil {
			continue
		}
		verdict := "NO DRIFT"
		if step.Drift.Drifted {
			verdict = "DRIFTED"
		}
		fmt.Fprintf(w, "\n%s  resource changes: %d  output changes: %d  resource drift: %d\n", verdict, step.Drift.ResourceChanges, step.Drift.OutputChanges, step.Drift.ResourceDrift)
		if step.Drift.Summary != "" {
			fmt.Fprintln(w, step.Drift.Summary)
		}
	}
}
