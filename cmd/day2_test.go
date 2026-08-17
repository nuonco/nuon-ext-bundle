package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nuonco/nuon/pkg/runner/airgap/day2"
	"github.com/nuonco/nuon/pkg/runner/airgap/day2state"
)

func writeDay2Object(t *testing.T, dir, key string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	path := filepath.Join(dir, filepath.FromSlash(key))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}

func testCatalog() day2.Catalog {
	return day2.Catalog{SchemaVersion: 1, DeploymentID: "dep-1", BundleDigest: "sha256:bundle", Refs: []day2.CatalogRef{{ID: "restart-api", Kind: day2.RefKindAction, Name: "Restart API", Component: "api", Steps: 2}}}
}

func TestPrintCatalog(t *testing.T) {
	var out bytes.Buffer
	catalog := testCatalog()
	require.NoError(t, printCatalog(&out, &catalog, nil, false))
	require.Contains(t, out.String(), "ID")
	require.Contains(t, out.String(), "restart-api")
	require.Contains(t, out.String(), "Restart API")
	require.Contains(t, out.String(), "api")
}

func TestFollowDispatchFinished(t *testing.T) {
	dir := t.TempDir()
	finished := time.Now().UTC()
	writeDay2Object(t, dir, day2.ClaimKey("cli-test"), day2.Claim{DispatchID: "cli-test", RunID: "run-1"})
	writeDay2Object(t, dir, day2.RunStatusKey("run-1"), day2.RunStatus{RunID: "run-1", Status: day2.RunStatusFinished, Steps: []day2.RunStep{{ID: "step-1", Name: "restart", JobID: "job-1", Status: "finished"}}})
	writeDay2Object(t, dir, day2.ReceiptKey("cli-test"), day2.Receipt{DispatchID: "cli-test", RunID: "run-1", Status: day2.ReceiptStatusFinished, FinishedAt: finished})
	var out bytes.Buffer
	require.NoError(t, followDispatch(context.Background(), &out, day2state.NewLocal(dir), "cli-test", time.Millisecond))
	require.Contains(t, out.String(), "restart")
	require.Contains(t, out.String(), "dispatch cli-test finished")
}

func TestRunsListAndDetail(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	writeDay2Object(t, dir, day2.RunStatusKey("old"), day2.RunStatus{RunID: "old", RefID: "one", StartedAt: now.Add(-time.Hour)})
	writeDay2Object(t, dir, day2.RunStatusKey("new"), day2.RunStatus{RunID: "new", RefID: "two", RefKind: day2.RefKindDrift, Status: day2.RunStatusFinished, StartedAt: now, FinishedAt: &now, Steps: []day2.RunStep{{ID: "drift", Name: "drift", Kind: "terraform", JobID: "job-new", Status: "finished", Drift: &day2.DriftResult{Drifted: true, ResourceChanges: 2}}}})
	runs, err := listRuns(context.Background(), day2state.NewLocal(dir))
	require.NoError(t, err)
	require.Equal(t, "new", runs[0].RunID)
	var list, detail bytes.Buffer
	require.NoError(t, printRuns(&list, runs, false))
	require.Contains(t, list.String(), "RUN ID")
	require.NoError(t, printRun(&detail, &runs[0], false, "state"))
	require.Contains(t, detail.String(), "DRIFTED")
	require.Contains(t, detail.String(), "nuon-bundle logs job-new")
}

func TestPortalSecurityAndAPI(t *testing.T) {
	dir := t.TempDir()
	writeDay2Object(t, dir, day2.CatalogKey, testCatalog())
	writeDay2Object(t, dir, day2.RunStatusKey("run-1"), day2.RunStatus{RunID: "run-1", StartedAt: time.Now().UTC()})
	p := &portalServer{store: day2state.NewLocal(dir), csrf: "secret", requestedBy: "operator", hosts: map[string]bool{"127.0.0.1:1234": true}}
	h := p.handler()

	request := httptest.NewRequest(http.MethodGet, "http://evil.example/api/catalog", nil)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1234/api/dispatch", strings.NewReader(`{"ref_id":"restart-api"}`))
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1234/api/dispatch", strings.NewReader(`{"ref_id":"restart-api"}`))
	request.Header.Set("X-CSRF-Token", "secret")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	var dispatched map[string]string
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &dispatched))
	raw, ok, err := p.store.Get(context.Background(), day2.RequestKey(dispatched["dispatch_id"]))
	require.NoError(t, err)
	require.True(t, ok)
	var saved day2.Request
	require.NoError(t, json.Unmarshal(raw, &saved))
	require.Equal(t, day2.SourcePortal, saved.Source)
	require.Equal(t, "operator", saved.RequestedBy)
	require.Equal(t, "sha256:bundle", saved.BundleDigest)
	require.ErrorIs(t, p.store.PutIfAbsent(context.Background(), day2.RequestKey(dispatched["dispatch_id"]), raw), errStateObjectExists)

	for _, path := range []string{"/api/catalog", "/api/runs", "/api/runs/run-1"} {
		request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234"+path, nil)
		response = httptest.NewRecorder()
		h.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		require.True(t, json.Valid(response.Body.Bytes()))
		require.Empty(t, response.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestLocalDay2StatePutIfAbsent(t *testing.T) {
	store := day2state.NewLocal(t.TempDir())
	require.NoError(t, store.PutIfAbsent(context.Background(), "x/y", []byte("one")))
	require.True(t, errors.Is(store.PutIfAbsent(context.Background(), "x/y", []byte("two")), errStateObjectExists))
}
