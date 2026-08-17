package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nuonco/nuon/pkg/runner/airgap"
)

func writeHealthState(t *testing.T, dir string, snapshot airgap.HealthSnapshot, transitions []airgap.HealthTransition) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "health"), 0o700))
	latest, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "health", "latest.json"), latest, 0o600))
	if transitions != nil {
		raw, err := json.Marshal(transitions)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "health", "transitions.json"), raw, 0o600))
	}
}

func TestHealthReaderFromFetcher(t *testing.T) {
	dir := t.TempDir()
	fetch, err := makeStateFetcher(context.Background(), dir, "", "")
	require.NoError(t, err)
	read := healthReaderFromFetcher(fetch)

	snapshot, transitions, err := read(context.Background())
	require.NoError(t, err)
	require.Nil(t, snapshot)
	require.Nil(t, transitions)

	writeHealthState(t, dir,
		airgap.HealthSnapshot{ObservedAt: time.Now().UTC(), Components: []airgap.ComponentHealth{{ComponentID: "cmp-1", ComponentName: "api", Health: "healthy"}}},
		[]airgap.HealthTransition{{ComponentName: "api", To: "healthy", ObservedAt: time.Now().UTC()}},
	)
	snapshot, transitions, err = read(context.Background())
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Components, 1)
	require.Len(t, transitions, 1)
}

func TestPrintHealthTable(t *testing.T) {
	var out bytes.Buffer
	snapshot := &airgap.HealthSnapshot{
		ObservedAt: time.Now().UTC(),
		Components: []airgap.ComponentHealth{
			{ComponentName: "api", ComponentType: "helm_chart", Health: "degraded", Resources: []airgap.ResourceHealth{
				{Kind: "Deployment", Health: "healthy"},
				{Kind: "Pod", Health: "degraded"},
			}},
		},
		SandboxReleases: []airgap.SandboxReleaseHealth{
			{ReleaseName: "ingress", Health: "healthy", Resources: []airgap.ResourceHealth{{Kind: "DaemonSet", Health: "healthy"}}},
		},
	}
	transitions := []airgap.HealthTransition{
		{ComponentName: "api", From: "healthy", To: "degraded", ObservedAt: time.Now().UTC()},
	}
	require.NoError(t, printHealth(&out, snapshot, transitions, false))
	text := out.String()
	require.Contains(t, text, "api")
	require.Contains(t, text, "degraded")
	require.Contains(t, text, "1 of 2 not healthy")
	require.Contains(t, text, "sandbox/ingress")
	require.Contains(t, text, "healthy -> degraded")
}

func TestPrintHealthJSON(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, printHealth(&out, &airgap.HealthSnapshot{}, nil, true))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Contains(t, decoded, "latest")
}

func TestFollowHealthPrintsTransitions(t *testing.T) {
	sequence := []struct {
		snapshot    *airgap.HealthSnapshot
		transitions []airgap.HealthTransition
	}{
		{nil, nil},
		{&airgap.HealthSnapshot{}, []airgap.HealthTransition{{ComponentName: "api", To: "healthy", ObservedAt: time.Now()}}},
		{&airgap.HealthSnapshot{}, []airgap.HealthTransition{
			{ComponentName: "api", To: "healthy", ObservedAt: time.Now()},
			{ComponentName: "api", From: "healthy", To: "degraded", ObservedAt: time.Now()},
		}},
	}
	i := 0
	ctx, cancel := context.WithCancel(context.Background())
	read := func(context.Context) (*airgap.HealthSnapshot, []airgap.HealthTransition, error) {
		entry := sequence[i]
		if i < len(sequence)-1 {
			i++
		} else {
			cancel()
		}
		return entry.snapshot, entry.transitions, nil
	}
	var out bytes.Buffer
	err := followHealth(ctx, &out, read, time.Millisecond, "state")
	require.ErrorIs(t, err, context.Canceled)
	text := out.String()
	require.Contains(t, text, "waiting for the first health report")
	require.Contains(t, text, "(new) -> healthy")
	require.Contains(t, text, "healthy -> degraded")
}
