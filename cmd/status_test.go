package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nuonco/nuon/pkg/runner/airgap/statestore"
)

func TestFollowStatusPrintsTransitionsAndFinishes(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	finished := time.Now()
	sequence := []*statestore.Status{
		nil,
		{RunID: "run-1", Status: statestore.RunStatusInProgress, Steps: []statestore.StepStatus{
			{ID: "sandbox", Status: "in-progress", StartedAt: &started},
		}},
		{RunID: "run-1", Status: statestore.RunStatusFinished, Steps: []statestore.StepStatus{
			{ID: "sandbox", Status: "finished", StartedAt: &started, FinishedAt: &finished},
			{ID: "deploy", Status: "finished", StartedAt: &started, FinishedAt: &finished},
		}},
	}
	i := 0
	read := func(context.Context) (*statestore.Status, error) {
		status := sequence[i]
		if i < len(sequence)-1 {
			i++
		}
		return status, nil
	}
	var out bytes.Buffer
	require.NoError(t, followStatus(context.Background(), &out, read, time.Millisecond, "s3://bucket/state", "s3://bucket/state"))
	text := out.String()
	require.Contains(t, text, "waiting for the runner to start")
	require.Contains(t, text, "in-progress")
	require.Contains(t, text, "deploy")
	require.Contains(t, text, "run run-1 finished")
}

func TestFollowStatusFailure(t *testing.T) {
	read := func(context.Context) (*statestore.Status, error) {
		return &statestore.Status{RunID: "run-2", Status: statestore.RunStatusFailed, FailedStep: "deploy", Steps: []statestore.StepStatus{
			{ID: "deploy", Status: statestore.RunStatusFailed, Error: "boom"},
		}}, nil
	}
	var out bytes.Buffer
	err := followStatus(context.Background(), &out, read, time.Millisecond, "state", "state")
	require.ErrorContains(t, err, "failed at job deploy")
	require.Contains(t, out.String(), "boom")
	require.Contains(t, out.String(), "nuon-bundle logs deploy --state state")
}

func TestPrintStatusTable(t *testing.T) {
	started := time.Now().Add(-30 * time.Second)
	var out bytes.Buffer
	require.NoError(t, printStatus(&out, &statestore.Status{
		InstallID: "inl1", RunID: "run-3", Status: statestore.RunStatusInProgress,
		Steps: []statestore.StepStatus{{ID: "job123", Name: "sandbox-terraform", Status: "in-progress", StartedAt: &started}},
	}, false, "s3://bucket/state"))
	text := out.String()
	require.Contains(t, text, "install inl1")
	require.Contains(t, text, "JOB ID")
	require.Contains(t, text, "job123")
	require.Contains(t, text, "sandbox-terraform")
	require.Contains(t, text, "in-progress")
	require.Contains(t, text, "nuon-bundle logs <job-id> --state s3://bucket/state")
}
