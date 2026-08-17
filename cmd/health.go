package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/runner/airgap"
)

type healthReader func(context.Context) (*airgap.HealthSnapshot, []airgap.HealthTransition, error)

func healthCmd() *cobra.Command {
	var state, profile, region string
	var follow, jsonOutput bool
	var interval time.Duration
	c := &cobra.Command{
		Use:   "health [--state <dir|s3://bucket/prefix>]",
		Short: "show component health reported by a resident offline runner",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := resolveStateFlags(&state, &profile, &region); err != nil {
				return err
			}
			fetch, err := makeStateFetcher(cmd.Context(), state, profile, region)
			if err != nil {
				return err
			}
			read := healthReaderFromFetcher(fetch)
			if !follow {
				snapshot, transitions, err := read(cmd.Context())
				if err != nil {
					return err
				}
				if snapshot == nil {
					return fmt.Errorf("no health reported at %s yet; the runner reports every minute once components are deployed", state)
				}
				return printHealth(cmd.OutOrStdout(), snapshot, transitions, jsonOutput)
			}
			return followHealth(cmd.Context(), cmd.OutOrStdout(), read, interval, state)
		},
	}
	c.Flags().StringVar(&state, "state", "", "runner state directory or s3://bucket/prefix state URI (defaults to the active context)")
	c.Flags().StringVar(&profile, "profile", "", "AWS profile for S3 state (defaults to the active context)")
	c.Flags().StringVar(&region, "region", "", "AWS region for S3 state (defaults to the active context)")
	c.Flags().BoolVar(&follow, "follow", false, "poll and print health transitions as they happen")
	c.Flags().BoolVar(&jsonOutput, "json", false, "print raw health JSON")
	c.Flags().DurationVar(&interval, "interval", 15*time.Second, "poll interval with --follow")
	return c
}

func healthReaderFromFetcher(fetch stateFetcher) healthReader {
	return func(ctx context.Context) (*airgap.HealthSnapshot, []airgap.HealthTransition, error) {
		raw, ok, err := fetch(ctx, "health/latest.json")
		if err != nil || !ok {
			return nil, nil, err
		}
		var snapshot airgap.HealthSnapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return nil, nil, fmt.Errorf("decode health/latest.json: %w", err)
		}
		var transitions []airgap.HealthTransition
		if raw, ok, err := fetch(ctx, "health/transitions.json"); err != nil {
			return nil, nil, err
		} else if ok {
			if err := json.Unmarshal(raw, &transitions); err != nil {
				return nil, nil, fmt.Errorf("decode health/transitions.json: %w", err)
			}
		}
		return &snapshot, transitions, nil
	}
}

func printHealth(w io.Writer, snapshot *airgap.HealthSnapshot, transitions []airgap.HealthTransition, jsonOutput bool) error {
	if jsonOutput {
		raw, err := json.MarshalIndent(map[string]any{"latest": snapshot, "transitions": transitions}, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(raw))
		return err
	}
	fmt.Fprintf(w, "observed %s\n", snapshot.ObservedAt.Local().Format(time.RFC3339))
	if snapshot.ClusterAccessError != "" {
		fmt.Fprintf(w, "cluster access error: %s\n", snapshot.ClusterAccessError)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "COMPONENT\tTYPE\tHEALTH\tRESOURCES")
	for _, c := range snapshot.Components {
		name := c.ComponentName
		if name == "" {
			name = c.ComponentID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, c.ComponentType, c.Health, summarizeResources(c.Resources))
	}
	for _, r := range snapshot.SandboxReleases {
		fmt.Fprintf(tw, "sandbox/%s\t%s\t%s\t%s\n", r.ReleaseName, "helm_release", r.Health, summarizeResources(r.Resources))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(transitions) == 0 {
		return nil
	}
	fmt.Fprintln(w, "\nrecent transitions:")
	start := 0
	if len(transitions) > 10 {
		start = len(transitions) - 10
	}
	for _, t := range transitions[start:] {
		from := t.From
		if from == "" {
			from = "(new)"
		}
		fmt.Fprintf(w, "  %s  %s: %s -> %s\n", t.ObservedAt.Local().Format(time.RFC3339), transitionName(t), from, t.To)
	}
	return nil
}

func transitionName(t airgap.HealthTransition) string {
	if t.ComponentName != "" {
		return t.ComponentName
	}
	return t.ComponentID
}

func summarizeResources(resources []airgap.ResourceHealth) string {
	if len(resources) == 0 {
		return ""
	}
	unhealthy := 0
	for _, r := range resources {
		if r.Health != "" && r.Health != "healthy" && r.Health != "not-applicable" {
			unhealthy++
		}
	}
	if unhealthy == 0 {
		return fmt.Sprintf("%d ok", len(resources))
	}
	return fmt.Sprintf("%d of %d not healthy", unhealthy, len(resources))
}

// Print transitions instead of redrawing so plain terminals retain history.
func followHealth(ctx context.Context, w io.Writer, read healthReader, interval time.Duration, state string) error {
	printed := 0
	var reportedWaiting bool
	for {
		snapshot, transitions, err := read(ctx)
		if err != nil {
			return err
		}
		switch {
		case snapshot == nil && !reportedWaiting:
			fmt.Fprintf(w, "%s waiting for the first health report at %s\n", time.Now().Format("15:04:05"), state)
			reportedWaiting = true
		case snapshot != nil:
			for ; printed < len(transitions); printed++ {
				t := transitions[printed]
				from := t.From
				if from == "" {
					from = "(new)"
				}
				fmt.Fprintf(w, "%s  %-32s %s -> %s\n", t.ObservedAt.Local().Format("15:04:05"), transitionName(t), from, t.To)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
