package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"

	"github.com/nuonco/nuon/pkg/config"
	"github.com/nuonco/nuon/pkg/runner/airgap"
	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

func TestPrintPlanEnvelopeInputs(t *testing.T) {
	raw, err := json.Marshal(airgap.Envelope{Version: "v0", OrgID: "org", AppID: "app", InstallID: "install", Inputs: []airgap.InputSpec{
		{Name: config.HelmValuesOverrideInputName("my-api"), Type: "yaml", Required: true, Bindable: true, Default: "replicas: 2"},
		{Name: "api_key", Type: "string", Secret: true},
		{Name: "region", Type: "string"},
	}})
	require.NoError(t, err)

	previous := os.Stdout
	read, write, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = write
	printPlanEnvelope(raw)
	require.NoError(t, write.Close())
	os.Stdout = previous
	output, err := io.ReadAll(read)
	require.NoError(t, err)
	require.NoError(t, read.Close())

	text := string(output)
	require.Contains(t, text, "NAME")
	require.Contains(t, text, "OFFLINE")
	require.Contains(t, text, "override:helm_values:my-api")
	require.Contains(t, text, "editable")
	require.Contains(t, text, "secret")
	require.Contains(t, text, "fixed at publish")
}

func TestInspectExtractPlan(t *testing.T) {
	envelope := `{"version":"v0","org_id":"org","app_id":"app","install_id":"install","steps":[{"id":"one","name":"one","job_type":"noop-deploy","job_operation":"exec","job_group":"deploy","composite_plan":{}}]}`
	var archive bytes.Buffer
	_, err := bundle.GenerateWithDocuments(context.Background(), &archive, bundle.LogicalManifest{
		SchemaVersion: 1,
		Target:        bundle.Target{OS: "linux", Architecture: "amd64"},
	}, bundle.Documents{PlanEnvelope: json.RawMessage(envelope)}, nil)
	require.NoError(t, err)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.zst")
	require.NoError(t, os.WriteFile(archivePath, archive.Bytes(), 0o644))

	extracted := filepath.Join(t.TempDir(), "plan.json")
	c := inspectCmd()
	c.SetContext(context.Background())
	c.SetArgs([]string{archivePath, "--extract-plan", extracted})
	require.NoError(t, c.Execute())

	contents, err := os.ReadFile(extracted)
	require.NoError(t, err)
	require.JSONEq(t, envelope, string(contents))
}

func TestInspectExtractRunner(t *testing.T) {
	ctx := context.Background()
	contents := []byte("#!/bin/sh\necho runner\n")
	store := memory.New()
	layer, err := oras.PushBytes(ctx, store, bundle.RunnerBinaryMediaType, contents)
	require.NoError(t, err)
	desc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, bundle.RunnerBinaryArtifactType, oras.PackManifestOptions{Layers: []ocispec.Descriptor{layer}})
	require.NoError(t, err)

	binary := bundle.Artifact{MediaType: desc.MediaType, Digest: desc.Digest.String(), Size: desc.Size}
	var archive bytes.Buffer
	_, err = bundle.Generate(ctx, &archive, bundle.LogicalManifest{
		SchemaVersion: 1,
		Target:        bundle.Target{OS: "linux", Architecture: "amd64"},
		Runner:        &bundle.Runner{Version: "v1", Binary: &binary},
	}, []bundle.Root{{Descriptor: desc, Source: store}})
	require.NoError(t, err)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.zst")
	require.NoError(t, os.WriteFile(archivePath, archive.Bytes(), 0o644))

	extracted := filepath.Join(t.TempDir(), "runner")
	c := inspectCmd()
	c.SetContext(ctx)
	c.SetArgs([]string{archivePath, "--extract-runner", extracted})
	require.NoError(t, c.Execute())

	got, err := os.ReadFile(extracted)
	require.NoError(t, err)
	require.Equal(t, contents, got)
	info, err := os.Stat(extracted)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestInspectExtractRunnerMissing(t *testing.T) {
	var archive bytes.Buffer
	_, err := bundle.Generate(context.Background(), &archive, bundle.LogicalManifest{
		SchemaVersion: 1,
		Target:        bundle.Target{OS: "linux", Architecture: "amd64"},
	}, nil)
	require.NoError(t, err)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.zst")
	require.NoError(t, os.WriteFile(archivePath, archive.Bytes(), 0o644))

	dest := filepath.Join(t.TempDir(), "runner")
	c := inspectCmd()
	c.SetContext(context.Background())
	c.SetArgs([]string{archivePath, "--extract-runner", dest})
	c.SilenceUsage, c.SilenceErrors = true, true
	require.ErrorContains(t, c.Execute(), "no runner binary")
	_, err = os.Stat(dest)
	require.True(t, os.IsNotExist(err))
}

func TestInspectExtractPlanMissingEnvelope(t *testing.T) {
	var archive bytes.Buffer
	_, err := bundle.Generate(context.Background(), &archive, bundle.LogicalManifest{
		SchemaVersion: 1,
		Target:        bundle.Target{OS: "linux", Architecture: "amd64"},
	}, nil)
	require.NoError(t, err)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.zst")
	require.NoError(t, os.WriteFile(archivePath, archive.Bytes(), 0o644))

	c := inspectCmd()
	c.SetContext(context.Background())
	c.SetArgs([]string{archivePath, "--extract-plan", filepath.Join(t.TempDir(), "plan.json")})
	c.SilenceUsage, c.SilenceErrors = true, true
	require.ErrorContains(t, c.Execute(), "no plan envelope")
}
