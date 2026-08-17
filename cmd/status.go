package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/runner/airgap/statestore"
)

type statusReader func(context.Context) (*statestore.Status, error)

func statusCmd() *cobra.Command {
	var state, profile, region string
	var follow, jsonOutput bool
	var interval time.Duration
	c := &cobra.Command{
		Use:   "status [--state <dir|s3://bucket/prefix>]",
		Short: "show live job status of an offline run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			explicitState := state
			if err := resolveStateFlags(&state, &profile, &region); err != nil {
				return err
			}
			read, err := makeStatusReader(cmd.Context(), state, profile, region)
			if err != nil {
				return err
			}
			if !follow {
				status, err := read(cmd.Context())
				if err != nil {
					return err
				}
				if status == nil {
					return fmt.Errorf("no run found at %s", state)
				}
				return printStatus(cmd.OutOrStdout(), status, jsonOutput, explicitState)
			}
			return followStatus(cmd.Context(), cmd.OutOrStdout(), read, interval, state, explicitState)
		},
	}
	c.Flags().StringVar(&state, "state", "", "runner state directory or s3://bucket/prefix state URI (defaults to the active context)")
	c.Flags().StringVar(&profile, "profile", "", "AWS profile for S3 state (defaults to the active context)")
	c.Flags().StringVar(&region, "region", "", "AWS region for S3 state (defaults to the active context)")
	c.Flags().BoolVar(&follow, "follow", false, "poll until the run finishes, printing job transitions")
	c.Flags().BoolVar(&jsonOutput, "json", false, "print raw status JSON")
	c.Flags().DurationVar(&interval, "interval", 10*time.Second, "poll interval with --follow")
	return c
}

func makeStatusReader(ctx context.Context, state, profile, region string) (statusReader, error) {
	fetch, err := makeStateFetcher(ctx, state, profile, region)
	if err != nil {
		return nil, err
	}
	return statusReaderFromFetcher(fetch), nil
}

func statusReaderFromFetcher(fetch stateFetcher) statusReader {
	return func(ctx context.Context) (*statestore.Status, error) {
		raw, ok, err := fetch(ctx, "status.json")
		if err != nil || !ok {
			return nil, err
		}
		var status statestore.Status
		if err := json.Unmarshal(raw, &status); err != nil {
			return nil, fmt.Errorf("decode status.json: %w", err)
		}
		return &status, nil
	}
}

// Missing state files return ok=false because writers publish them asynchronously.
type stateFetcher func(ctx context.Context, name string) ([]byte, bool, error)

func makeStateFetcher(ctx context.Context, state, profile, region string) (stateFetcher, error) {
	if !strings.HasPrefix(state, "s3://") {
		if _, err := os.Stat(state); err != nil {
			return nil, fmt.Errorf("unable to open state directory: %w", err)
		}
		return func(_ context.Context, name string) ([]byte, bool, error) {
			raw, err := os.ReadFile(filepath.Join(state, name))
			if errors.Is(err, os.ErrNotExist) {
				return nil, false, nil
			}
			if err != nil {
				return nil, false, err
			}
			return raw, true, nil
		}, nil
	}
	trimmed := strings.TrimPrefix(state, "s3://")
	bucket, prefix, _ := strings.Cut(trimmed, "/")
	if bucket == "" {
		return nil, fmt.Errorf("state S3 URI bucket is required")
	}
	keyPrefix := strings.Trim(prefix, "/")
	if keyPrefix != "" {
		keyPrefix += "/"
	}
	options := []func(*config.LoadOptions) error{}
	if profile != "" {
		options = append(options, config.WithSharedConfigProfile(profile))
	}
	if region != "" {
		options = append(options, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %w", err)
	}
	client := s3.NewFromConfig(cfg)
	return func(ctx context.Context, name string) ([]byte, bool, error) {
		key := keyPrefix + name
		out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			var noKey *types.NoSuchKey
			if errors.As(err, &noKey) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("get s3://%s/%s: %w", bucket, key, err)
		}
		defer out.Body.Close()
		raw, err := io.ReadAll(out.Body)
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	}, nil
}

func printStatus(w io.Writer, status *statestore.Status, jsonOutput bool, state string) error {
	if jsonOutput {
		raw, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(raw))
		return err
	}
	fmt.Fprintf(w, "install %s  run %s  status %s", status.InstallID, status.RunID, status.Status)
	if status.FailedStep != "" {
		fmt.Fprintf(w, "  failed job %s", status.FailedStep)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB ID\tNAME\tSTATUS\tDURATION")
	for _, step := range status.Steps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", step.ID, step.Name, step.Status, stepDuration(step))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\nlogs: %s\n", logsCommandHint("<job-id>", state))
	return err
}

func logsCommandHint(jobID, explicitState string) string {
	hint := "nuon-bundle logs " + jobID
	if explicitState != "" {
		hint += " --state " + explicitState
	}
	return hint
}

func stepDuration(step statestore.StepStatus) string {
	if step.StartedAt == nil {
		return ""
	}
	end := time.Now()
	if step.FinishedAt != nil {
		end = *step.FinishedAt
	}
	return end.Sub(*step.StartedAt).Round(time.Second).String()
}

// Print transitions instead of redrawing so plain terminals retain run history.
func followStatus(ctx context.Context, w io.Writer, read statusReader, interval time.Duration, state, explicitState string) error {
	seen := map[string]string{}
	var reportedWaiting bool
	for {
		status, err := read(ctx)
		if err != nil {
			return err
		}
		switch {
		case status == nil && !reportedWaiting:
			fmt.Fprintf(w, "%s waiting for the runner to start (no status at %s yet)\n", time.Now().Format("15:04:05"), state)
			reportedWaiting = true
		case status != nil:
			for _, step := range status.Steps {
				if seen[step.ID] == step.Status {
					continue
				}
				seen[step.ID] = step.Status
				line := fmt.Sprintf("%s  %-44s %s", time.Now().Format("15:04:05"), step.ID, step.Status)
				if step.Status == statestore.RunStatusFailed && step.Error != "" {
					line += "  " + step.Error
				}
				if step.FinishedAt != nil && step.StartedAt != nil {
					line += "  (" + stepDuration(step) + ")"
				}
				fmt.Fprintln(w, line)
			}
			if status.Status == statestore.RunStatusFinished {
				fmt.Fprintf(w, "\nrun %s finished\n", status.RunID)
				return nil
			}
			if status.Status == statestore.RunStatusFailed {
				if status.FailedStep != "" {
					fmt.Fprintf(w, "\nlogs: %s\n", logsCommandHint(status.FailedStep, explicitState))
				}
				return fmt.Errorf("run %s failed at job %s", status.RunID, status.FailedStep)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
