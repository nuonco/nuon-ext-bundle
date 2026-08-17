package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nuonco/nuon/pkg/runner/airgap/statestore"
)

func TestFormatLogLine(t *testing.T) {
	line := []byte(`{"level":"info","ts":1754500000.5,"caller":"jobloop/exec_job.go:64","msg":"creating job execution","runner_job.id":"job123"}`)
	text := formatLogLine(line)
	require.Contains(t, text, "INFO")
	require.Contains(t, text, "creating job execution")
	require.Contains(t, text, "runner_job.id=job123")
	require.NotContains(t, text, "caller")

	require.Equal(t, "not json", formatLogLine([]byte("not json")))
}

func TestPrintLogLinesRawJSON(t *testing.T) {
	raw := []byte("{\"msg\":\"a\"}\n{\"msg\":\"b\"}\n")
	var out bytes.Buffer
	require.NoError(t, printLogLines(&out, raw, true))
	require.Equal(t, "{\"msg\":\"a\"}\n{\"msg\":\"b\"}\n", out.String())
}

func TestFollowJobLogsPrintsNewCompleteLines(t *testing.T) {
	fetches := [][]byte{
		nil,
		[]byte("{\"msg\":\"one\"}\n{\"msg\":\"tw"),
		[]byte("{\"msg\":\"one\"}\n{\"msg\":\"two\"}\n"),
		[]byte("{\"msg\":\"one\"}\n{\"msg\":\"two\"}\n"),
	}
	i := 0
	fetchLog := func(context.Context) ([]byte, bool, error) {
		raw := fetches[i]
		if i < len(fetches)-1 {
			i++
		}
		if raw == nil {
			return nil, false, nil
		}
		return raw, true, nil
	}
	readStatus := func(context.Context) (*statestore.Status, error) {
		step := statestore.StepStatus{ID: "job123", Status: "in-progress"}
		if i >= len(fetches)-1 {
			step.Status = "finished"
		}
		return &statestore.Status{RunID: "run-1", Steps: []statestore.StepStatus{step}}, nil
	}
	var out bytes.Buffer
	require.NoError(t, followJobLogs(context.Background(), &out, fetchLog, readStatus, "job123", time.Millisecond, false))
	text := out.String()
	require.Contains(t, text, "waiting for logs")
	require.Contains(t, text, "one")
	require.Contains(t, text, "two")
	require.Contains(t, text, "job job123 finished")
	require.Less(t, len([]byte(text)), 1000)
}

func TestFollowJobLogsFailedJobPrintsError(t *testing.T) {
	fetchLog := func(context.Context) ([]byte, bool, error) {
		return []byte("{\"msg\":\"boom\"}\n"), true, nil
	}
	readStatus := func(context.Context) (*statestore.Status, error) {
		return &statestore.Status{RunID: "run-2", Steps: []statestore.StepStatus{
			{ID: "job9", Status: "failed", Error: "terraform apply failed"},
		}}, nil
	}
	var out bytes.Buffer
	require.NoError(t, followJobLogs(context.Background(), &out, fetchLog, readStatus, "job9", time.Millisecond, false))
	text := out.String()
	require.Contains(t, text, "boom")
	require.Contains(t, text, "job job9 failed")
	require.Contains(t, text, "terraform apply failed")
}

func TestFindStepAndJobIDs(t *testing.T) {
	status := &statestore.Status{Steps: []statestore.StepStatus{{ID: "job1"}, {ID: "job2"}}}
	require.NotNil(t, findStep(status, "job1"))
	require.Nil(t, findStep(status, "job3"))
	require.Equal(t, []string{"job1", "job2"}, jobIDs(status))
}
