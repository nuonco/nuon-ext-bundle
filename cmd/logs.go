package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/runner/airgap/statestore"
	"github.com/nuonco/nuon/sdks/nuon-go/models"
)

func logsCmd() *cobra.Command {
	var state, profile, region string
	var follow, jsonOutput bool
	var interval time.Duration
	c := &cobra.Command{
		Use:   "logs <job-id> [--state <dir|s3://bucket/prefix>]",
		Short: "show a job's logs from an offline run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			if err := resolveStateFlags(&state, &profile, &region); err != nil {
				return err
			}
			fetch, err := makeStateFetcher(cmd.Context(), state, profile, region)
			if err != nil {
				return err
			}
			readStatus := statusReaderFromFetcher(fetch)
			status, err := readStatus(cmd.Context())
			if err != nil {
				return err
			}
			if status == nil {
				return fmt.Errorf("no run found at %s", state)
			}
			// Restrict log keys to job IDs declared by status.json.
			if findStep(status, jobID) == nil {
				return fmt.Errorf("unknown job %q; jobs in this run:\n  %s", jobID, strings.Join(jobIDs(status), "\n  "))
			}
			fetchLog := func(ctx context.Context) ([]byte, bool, error) {
				return fetch(ctx, "job-logs/"+jobID+".ndjson")
			}
			w := cmd.OutOrStdout()
			if !follow {
				raw, ok, err := fetchLog(cmd.Context())
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintf(w, "no logs for %s yet (the runner syncs state to S3 every ~30s)\n", jobID)
					return nil
				}
				return printLogLines(w, raw, jsonOutput)
			}
			return followJobLogs(cmd.Context(), w, fetchLog, readStatus, jobID, interval, jsonOutput)
		},
	}
	c.Flags().StringVar(&state, "state", "", "runner state directory or s3://bucket/prefix state URI (defaults to the active context)")
	c.Flags().StringVar(&profile, "profile", "", "AWS profile for S3 state (defaults to the active context)")
	c.Flags().StringVar(&region, "region", "", "AWS region for S3 state (defaults to the active context)")
	c.Flags().BoolVar(&follow, "follow", false, "poll for new log lines until the job finishes")
	c.Flags().BoolVar(&jsonOutput, "json", false, "print raw NDJSON records")
	c.Flags().DurationVar(&interval, "interval", 10*time.Second, "poll interval with --follow")
	return c
}

func findStep(status *statestore.Status, jobID string) *statestore.StepStatus {
	for i := range status.Steps {
		if status.Steps[i].ID == jobID {
			return &status.Steps[i]
		}
	}
	return nil
}

func jobIDs(status *statestore.Status) []string {
	ids := make([]string, 0, len(status.Steps))
	for _, step := range status.Steps {
		ids = append(ids, step.ID)
	}
	return ids
}

func printLogLines(w io.Writer, raw []byte, jsonOutput bool) error {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if jsonOutput {
			if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintln(w, formatLogLine(line)); err != nil {
			return err
		}
	}
	return nil
}

func formatLogLine(line []byte) string {
	var entry map[string]any
	if err := json.Unmarshal(line, &entry); err != nil {
		return string(line)
	}
	ts := ""
	if v, ok := entry["ts"].(float64); ok {
		ts = time.Unix(int64(v), 0).UTC().Format("15:04:05")
	}
	level, _ := entry["level"].(string)
	msg, _ := entry["msg"].(string)
	for _, k := range []string{"ts", "level", "msg", "caller"} {
		delete(entry, k)
	}
	keys := make([]string, 0, len(entry))
	for k := range entry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %s", ts, strings.ToUpper(level), msg)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s=%v", k, entry[k])
	}
	return b.String()
}

func isTerminalJobStatus(status string) bool {
	switch status {
	case string(models.AppRunnerJobStatusFinished), string(models.AppRunnerJobStatusFailed), string(models.AppRunnerJobStatusCancelled):
		return true
	}
	return false
}

// Hold trailing partial lines because an S3 sync can capture a file mid-write.
func followJobLogs(ctx context.Context, w io.Writer, fetchLog func(context.Context) ([]byte, bool, error), readStatus statusReader, jobID string, interval time.Duration, jsonOutput bool) error {
	var offset int
	var reportedWaiting bool
	for {
		raw, ok, err := fetchLog(ctx)
		if err != nil {
			return err
		}
		printedNew := false
		if ok && len(raw) > offset {
			data := raw[offset:]
			if idx := bytes.LastIndexByte(data, '\n'); idx >= 0 {
				if err := printLogLines(w, data[:idx+1], jsonOutput); err != nil {
					return err
				}
				offset += idx + 1
				printedNew = true
			}
		}
		if !ok && !reportedWaiting {
			fmt.Fprintf(w, "waiting for logs for %s (the runner syncs state to S3 every ~30s)\n", jobID)
			reportedWaiting = true
		}
		status, err := readStatus(ctx)
		if err != nil {
			return err
		}
		if status != nil {
			if step := findStep(status, jobID); step != nil && isTerminalJobStatus(step.Status) && !printedNew {
				fmt.Fprintf(w, "\njob %s %s\n", jobID, step.Status)
				if step.Status == string(models.AppRunnerJobStatusFailed) && step.Error != "" {
					fmt.Fprintf(w, "error: %s\n", step.Error)
				}
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
