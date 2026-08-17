package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func testPaths(t *testing.T) configPaths {
	t.Helper()
	dir := t.TempDir()
	paths := configPaths{
		active:   filepath.Join(dir, ".nuon-bundle"),
		contexts: filepath.Join(dir, "contexts"),
	}
	t.Setenv("NUON_BUNDLE_CONFIG", paths.active)
	t.Setenv("NUON_BUNDLE_CONTEXTS_DIR", paths.contexts)
	return paths
}

func TestConfigLoadMissingIsEmpty(t *testing.T) {
	testPaths(t)
	cfg, err := loadBundleConfig()
	require.NoError(t, err)
	require.Equal(t, &bundleConfig{}, cfg)
}

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	testPaths(t)
	want := &bundleConfig{
		Bucket: "customer-bucket", BucketPrefix: "install/", Region: "us-east-1",
		Profile: "customer-admin", DeploymentID: "demo2",
		State: "s3://customer-bucket/install/demo2/state/",
	}
	require.NoError(t, saveBundleConfig(want))
	got, err := loadBundleConfig()
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestConfigSaveWritesThroughContextSymlink(t *testing.T) {
	paths := testPaths(t)
	require.NoError(t, saveBundleConfig(&bundleConfig{Bucket: "b1"}))
	require.NoError(t, saveContext(paths, "prod"))

	require.NoError(t, saveBundleConfig(&bundleConfig{Bucket: "b2"}))

	raw, err := os.ReadFile(filepath.Join(paths.contexts, "prod"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "b2")
	cfg, err := loadBundleConfig()
	require.NoError(t, err)
	require.Equal(t, "b2", cfg.Bucket)
}

func TestCtxSaveSwitchPreviousDelete(t *testing.T) {
	paths := testPaths(t)
	require.NoError(t, saveBundleConfig(&bundleConfig{Bucket: "a"}))
	require.NoError(t, saveContext(paths, "customer-a"))

	name, err := currentContext(paths)
	require.NoError(t, err)
	require.Equal(t, "customer-a", name)

	require.NoError(t, copyContext(paths, "customer-b", filepath.Join(paths.contexts, "customer-a")))
	require.NoError(t, switchContext(paths, "customer-b"))
	name, err = currentContext(paths)
	require.NoError(t, err)
	require.Equal(t, "customer-b", name)

	require.NoError(t, switchPrevious(paths))
	name, err = currentContext(paths)
	require.NoError(t, err)
	require.Equal(t, "customer-a", name)

	var out bytes.Buffer
	require.NoError(t, listContexts(&out, paths))
	require.Contains(t, out.String(), "* customer-a")
	require.Contains(t, out.String(), "  customer-b")

	require.NoError(t, deleteContexts(paths, []string{"."}))
	_, err = os.Lstat(paths.active)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(paths.contexts, "customer-a"))
	require.True(t, os.IsNotExist(err))
}

func TestCtxSaveRejectsExisting(t *testing.T) {
	paths := testPaths(t)
	require.NoError(t, saveBundleConfig(&bundleConfig{}))
	require.NoError(t, saveContext(paths, "prod"))
	require.NoError(t, saveBundleConfig(&bundleConfig{}))
	require.ErrorContains(t, saveContext(paths, "prod"), "already exists")
}

func TestCtxRenameActive(t *testing.T) {
	paths := testPaths(t)
	require.NoError(t, saveBundleConfig(&bundleConfig{Bucket: "x"}))
	require.NoError(t, saveContext(paths, "old"))
	require.NoError(t, renameContext(paths, "new", "."))
	name, err := currentContext(paths)
	require.NoError(t, err)
	require.Equal(t, "new", name)
	cfg, err := loadBundleConfig()
	require.NoError(t, err)
	require.Equal(t, "x", cfg.Bucket)
}

func TestCtxSwitchRefusesRegularFile(t *testing.T) {
	paths := testPaths(t)
	require.NoError(t, os.MkdirAll(paths.contexts, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(paths.contexts, "prod"), []byte("bucket: b\n"), 0o600))
	require.NoError(t, os.WriteFile(paths.active, []byte("bucket: unsaved\n"), 0o600))
	require.ErrorContains(t, switchContext(paths, "prod"), "save it first")
}

func TestCtxInvalidNames(t *testing.T) {
	paths := testPaths(t)
	for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
		_, err := contextPath(paths, name)
		require.Error(t, err, name)
	}
}

func TestResolveStateFlagsFromConfig(t *testing.T) {
	testPaths(t)
	require.NoError(t, saveBundleConfig(&bundleConfig{
		State: "s3://bucket/state/", Profile: "customer-admin", Region: "us-east-1",
	}))
	var state, profile, region string
	require.NoError(t, resolveStateFlags(&state, &profile, &region))
	require.Equal(t, "s3://bucket/state/", state)
	require.Equal(t, "customer-admin", profile)
	require.Equal(t, "us-east-1", region)

	state, profile, region = "/tmp/other", "other-profile", "us-west-2"
	require.NoError(t, resolveStateFlags(&state, &profile, &region))
	require.Equal(t, "/tmp/other", state)
	require.Equal(t, "other-profile", profile)
	require.Equal(t, "us-west-2", region)
}

func TestResolveStateFlagsErrorsWithoutState(t *testing.T) {
	testPaths(t)
	var state, profile, region string
	require.ErrorContains(t, resolveStateFlags(&state, &profile, &region), "no state configured")
}
