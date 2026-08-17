package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/user"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/runner/airgap/day2"
)

func runCmd() *cobra.Command {
	var state, profile, region, dispatchID, requestedBy string
	var follow, noWait bool
	c := &cobra.Command{Use: "run <ref-id>", Short: "dispatch and follow a day-2 operation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := resolveStateFlags(&state, &profile, &region); err != nil {
			return err
		}
		store, err := makeDay2State(cmd.Context(), state, profile, region)
		if err != nil {
			return err
		}
		catalog, _, err := readCatalog(cmd.Context(), store)
		if err != nil {
			return err
		}
		if !catalogHasRef(catalog, args[0]) {
			return fmt.Errorf("unknown ref %q; available refs:\n  %s", args[0], strings.Join(catalogRefIDs(catalog), "\n  "))
		}
		if dispatchID == "" {
			dispatchID = "cli-" + strings.ToLower(ulid.Make().String())
		}
		if err := day2.ValidateDispatchID(dispatchID); err != nil {
			return err
		}
		if requestedBy == "" {
			requestedBy = currentUsername()
		}
		req := day2.Request{SchemaVersion: day2.SchemaVersion, DeploymentID: catalog.DeploymentID, BundleDigest: catalog.BundleDigest, RefID: args[0], DispatchID: dispatchID, Source: day2.SourceCLI, RequestedBy: requestedBy, CreatedAt: time.Now().UTC()}
		raw, err := json.Marshal(req)
		if err != nil {
			return err
		}
		err = store.PutIfAbsent(cmd.Context(), day2.RequestKey(dispatchID), raw)
		if errors.Is(err, errStateObjectExists) {
			fmt.Fprintf(cmd.OutOrStdout(), "dispatch %s already exists; following it\n", dispatchID)
		} else if err != nil {
			return err
		}
		if noWait {
			fmt.Fprintln(cmd.OutOrStdout(), dispatchID)
			return nil
		}
		return followDispatch(cmd.Context(), cmd.OutOrStdout(), store, dispatchID, 3*time.Second)
	}}
	stateFlags(c, &state, &profile, &region)
	c.Flags().StringVar(&dispatchID, "dispatch-id", "", "dispatch ID (defaults to cli-<ulid>)")
	c.Flags().StringVar(&requestedBy, "requested-by", "", "display name for the requester (defaults to the OS username)")
	c.Flags().BoolVar(&follow, "follow", false, "wait for the operation to finish (the default unless --no-wait is set)")
	c.Flags().BoolVar(&noWait, "no-wait", false, "dispatch and print the dispatch ID without waiting")
	return c
}

func currentUsername() string {
	current, err := user.Current()
	if err == nil {
		return current.Username
	}
	return ""
}
func catalogHasRef(c *day2.Catalog, id string) bool {
	for _, ref := range c.Refs {
		if ref.ID == id {
			return true
		}
	}
	return false
}
func catalogRefIDs(c *day2.Catalog) []string {
	ids := make([]string, 0, len(c.Refs))
	for _, ref := range c.Refs {
		ids = append(ids, ref.ID)
	}
	return ids
}

func followDispatch(ctx context.Context, w io.Writer, store day2State, dispatchID string, interval time.Duration) error {
	seen := map[string]string{}
	var runID string
	for {
		if raw, ok, err := store.Get(ctx, day2.ReceiptKey(dispatchID)); err != nil {
			return err
		} else if ok {
			var receipt day2.Receipt
			if err := json.Unmarshal(raw, &receipt); err != nil {
				return err
			}
			if receipt.RunID != "" {
				runID = receipt.RunID
				if run, err := readRun(ctx, store, runID); err != nil {
					return err
				} else if run != nil {
					printRunTransitions(w, run, seen)
				}
			}
			fmt.Fprintf(w, "\ndispatch %s %s", dispatchID, receipt.Status)
			if receipt.Reason != "" {
				fmt.Fprintf(w, ": %s", receipt.Reason)
			}
			fmt.Fprintln(w)
			if receipt.Status == day2.ReceiptStatusFinished {
				return nil
			}
			return fmt.Errorf("dispatch %s %s", dispatchID, receipt.Status)
		}
		if runID == "" {
			if raw, ok, err := store.Get(ctx, day2.ClaimKey(dispatchID)); err != nil {
				return err
			} else if ok {
				var claim day2.Claim
				if err := json.Unmarshal(raw, &claim); err != nil {
					return err
				}
				runID = claim.RunID
				fmt.Fprintf(w, "%s  runner claimed dispatch; run %s\n", time.Now().Format("15:04:05"), runID)
			}
		}
		if runID != "" {
			if run, err := readRun(ctx, store, runID); err != nil {
				return err
			} else if run != nil {
				printRunTransitions(w, run, seen)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func printRunTransitions(w io.Writer, run *day2.RunStatus, seen map[string]string) {
	for _, step := range run.Steps {
		if seen[step.ID] == step.Status {
			continue
		}
		seen[step.ID] = step.Status
		fmt.Fprintf(w, "%s  %-44s %s", time.Now().Format("15:04:05"), step.Name, step.Status)
		if step.JobID != "" {
			fmt.Fprintf(w, "  %s", step.JobID)
		}
		if step.Error != "" {
			fmt.Fprintf(w, "  %s", step.Error)
		}
		fmt.Fprintln(w)
		if step.Drift != nil {
			verdict := "NO DRIFT"
			if step.Drift.Drifted {
				verdict = "DRIFTED"
			}
			fmt.Fprintf(w, "%s  resource changes: %d  output changes: %d  resource drift: %d\n", verdict, step.Drift.ResourceChanges, step.Drift.OutputChanges, step.Drift.ResourceDrift)
		}
	}
}
