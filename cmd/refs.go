package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/runner/airgap/day2"
)

func refsCmd() *cobra.Command {
	var state, profile, region string
	var jsonOutput bool
	c := &cobra.Command{Use: "refs", Short: "list day-2 operations published by the resident runner", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := resolveStateFlags(&state, &profile, &region); err != nil {
			return err
		}
		store, err := makeDay2State(cmd.Context(), state, profile, region)
		if err != nil {
			return err
		}
		catalog, raw, err := readCatalog(cmd.Context(), store)
		if err != nil {
			return err
		}
		return printCatalog(cmd.OutOrStdout(), catalog, raw, jsonOutput)
	}}
	stateFlags(c, &state, &profile, &region)
	c.Flags().BoolVar(&jsonOutput, "json", false, "print raw catalog JSON")
	return c
}

func stateFlags(c *cobra.Command, state, profile, region *string) {
	c.Flags().StringVar(state, "state", "", "runner state directory or s3://bucket/prefix state URI (defaults to the active context)")
	c.Flags().StringVar(profile, "profile", "", "AWS profile for S3 state (defaults to the active context)")
	c.Flags().StringVar(region, "region", "", "AWS region for S3 state (defaults to the active context)")
}

func readCatalog(ctx context.Context, store day2State) (*day2.Catalog, []byte, error) {
	raw, ok, err := store.Get(ctx, day2.CatalogKey)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("no day-2 catalog found; the resident runner publishes it after bootstrap")
	}
	var catalog day2.Catalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, nil, fmt.Errorf("decode %s: %w", day2.CatalogKey, err)
	}
	return &catalog, raw, nil
}

func printCatalog(w io.Writer, catalog *day2.Catalog, raw []byte, jsonOutput bool) error {
	if jsonOutput {
		_, err := fmt.Fprintln(w, string(raw))
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tNAME\tCOMPONENT\tCRON\tSTEPS")
	for _, ref := range catalog.Refs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", ref.ID, ref.Kind, ref.Name, ref.Component, ref.CronSchedule, ref.Steps)
	}
	return tw.Flush()
}
