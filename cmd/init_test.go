package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

func runInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := initCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestInitFromFileCreatesAndActivatesNamedContext(t *testing.T) {
	paths := testPaths(t)
	file := writeConfigFile(t, `
ecr_registry: 111122223333.dkr.ecr.us-east-1.amazonaws.com
ecr_prefix: acme
bucket: acme-nuon-install
bucket_prefix: install/
region: us-east-1
profile: customer-admin
install_id: inla9i0ykq7os0la04zflj3ubv
`)
	out, err := runInit(t, "--name", "acme", "--file", file)
	require.NoError(t, err)
	require.Contains(t, out, `created and activated context "acme"`)

	name, err := currentContext(paths)
	require.NoError(t, err)
	require.Equal(t, "acme", name)

	cfg, err := loadBundleConfig()
	require.NoError(t, err)
	require.Equal(t, "111122223333.dkr.ecr.us-east-1.amazonaws.com", cfg.ECRRegistry)
	require.Equal(t, "acme", cfg.ECRPrefix)
	require.Equal(t, "acme-nuon-install", cfg.Bucket)
	require.Equal(t, "install/", cfg.BucketPrefix)
	require.Equal(t, "us-east-1", cfg.Region)
	require.Equal(t, "customer-admin", cfg.Profile)
	require.Equal(t, "inla9i0ykq7os0la04zflj3ubv", cfg.InstallID)

	info, err := os.Stat(filepath.Join(paths.contexts, "acme"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestInitNamedContextAlreadyExists(t *testing.T) {
	testPaths(t)
	file := writeConfigFile(t, "bucket: b\n")
	_, err := runInit(t, "--name", "acme", "--file", file)
	require.NoError(t, err)
	_, err = runInit(t, "--name", "acme", "--file", file)
	require.ErrorContains(t, err, `context "acme" already exists`)
}

func TestInitNamedContextRecordsPrevious(t *testing.T) {
	paths := testPaths(t)
	_, err := runInit(t, "--name", "customer-a", "--file", writeConfigFile(t, "bucket: a\n"))
	require.NoError(t, err)
	_, err = runInit(t, "--name", "customer-b", "--file", writeConfigFile(t, "bucket: b\n"))
	require.NoError(t, err)

	name, err := currentContext(paths)
	require.NoError(t, err)
	require.Equal(t, "customer-b", name)

	require.NoError(t, switchPrevious(paths))
	name, err = currentContext(paths)
	require.NoError(t, err)
	require.Equal(t, "customer-a", name)
}

func TestInitFileWithoutNameReplacesActiveConfig(t *testing.T) {
	testPaths(t)
	require.NoError(t, saveBundleConfig(&bundleConfig{Bucket: "old-bucket", State: "s3://old/state/"}))

	out, err := runInit(t, "--file", writeConfigFile(t, "bucket: new-bucket\n"))
	require.NoError(t, err)
	require.Contains(t, out, "updated")

	cfg, err := loadBundleConfig()
	require.NoError(t, err)
	require.Equal(t, "new-bucket", cfg.Bucket)
	require.Empty(t, cfg.State)
}

func TestInitNamedRefusesToClobberUnsavedConfig(t *testing.T) {
	testPaths(t)
	require.NoError(t, saveBundleConfig(&bundleConfig{Bucket: "unsaved"}))

	_, err := runInit(t, "--name", "acme", "--file", writeConfigFile(t, "bucket: acme-bucket\n"))
	require.ErrorContains(t, err, "save it first")

	cfg, err := loadBundleConfig()
	require.NoError(t, err)
	require.Equal(t, "unsaved", cfg.Bucket)
}

func TestInitRejectsInvalidContextName(t *testing.T) {
	testPaths(t)
	_, err := runInit(t, "--name", "bad/name", "--file", writeConfigFile(t, "bucket: b\n"))
	require.ErrorContains(t, err, "invalid context name")
}

func TestInitFileRejectsUnknownKeys(t *testing.T) {
	testPaths(t)
	file := writeConfigFile(t, "ecr_registery: typo.example.com\n")
	_, err := runInit(t, "--name", "acme", "--file", file)
	require.ErrorContains(t, err, "ecr_registery")
}

func TestInitFileMissing(t *testing.T) {
	testPaths(t)
	_, err := runInit(t, "--name", "acme", "--file", "/nonexistent/config.yaml")
	require.ErrorContains(t, err, "read /nonexistent/config.yaml")
}

func TestInitNoFileNonTTYErrors(t *testing.T) {
	testPaths(t)
	_, err := runInit(t)
	require.ErrorContains(t, err, "pass --file")
}

func memberWithKey(key string) bundle.Member {
	return bundle.Member{Key: key}
}

func TestRunnerImageRef(t *testing.T) {
	items := []planItem{
		{Member: memberWithKey("sandbox:eks"), Repo: "acme/sandbox", Tag: "sha256-aaa", Status: "pushed"},
		{Member: memberWithKey("runner:image"), Repo: "acme/runner", Tag: "sha256-bbb", Status: "pushed"},
	}
	require.Equal(t, "reg.example.com/acme/runner:sha256-bbb", runnerImageRef("reg.example.com", items))

	items[1].Status = "skipped"
	require.Equal(t, "reg.example.com/acme/runner:sha256-bbb", runnerImageRef("reg.example.com", items))

	items[1].Status = "failed"
	require.Equal(t, "", runnerImageRef("reg.example.com", items))
}
