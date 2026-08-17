package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nuonco/nuon/pkg/config"
	"github.com/nuonco/nuon/pkg/runner/airgap"
)

func TestRenderInitAirgap(t *testing.T) {
	script, err := renderInitAirgap(stackTemplateData{
		Bucket: "customer-bucket", Prefix: "install/", Region: "us-east-1",
		ImageURL: "123.dkr.ecr.us-east-1.amazonaws.com/nuon/runner", ImageTag: "v1",
		ECRRegistry: "123.dkr.ecr.us-east-1.amazonaws.com", BundleKey: "bundle/test.tar.zst",
		StatePrefix: "s3://customer-bucket/install/state", RunnerID: "runner-1",
	})
	require.NoError(t, err)
	text := string(script)
	for _, expected := range []string{
		"RUNNER_ID=runner-1", "AWS_REGION=us-east-1", "NUON_AIRGAP=true",
		"AIRGAP_BUNDLE_URI=s3://customer-bucket/install/bundle/test.tar.zst",
		"AIRGAP_STATE_URI=s3://customer-bucket/install/state",
		"AIRGAP_ECR_REGISTRY=123.dkr.ecr.us-east-1.amazonaws.com",
		"AIRGAP_WORKDIR=/tmp/airgap", "ExecStart=/opt/nuon/runner/bin/runner mng airgap",
	} {
		require.Contains(t, text, expected)
	}
	for _, forbidden := range []string{"nuon-artifacts.s3", "public-settings", "checkip"} {
		require.NotContains(t, text, forbidden)
	}
}

func TestRenderInitAirgapDeploymentID(t *testing.T) {
	data := stackTemplateData{
		Bucket: "b", Prefix: "p/", Region: "us-east-1", ImageURL: "123.dkr.ecr.us-east-1.amazonaws.com/r",
		ImageTag: "v1", ECRRegistry: "123.dkr.ecr.us-east-1.amazonaws.com", BundleKey: "bundle/t.tar.zst",
		StatePrefix: "s3://b/p/state", StackOutputsURI: "s3://b/p/stack-outputs/outputs.json", RunnerID: "r1",
	}
	script, err := renderInitAirgap(data)
	require.NoError(t, err)
	require.NotContains(t, string(script), "AIRGAP_DEPLOYMENT_ID")
	require.NotContains(t, string(script), "AIRGAP_INSTALL_INPUTS_URI")

	data.DeploymentID = "demo2"
	data.InstallInputsURI = "s3://b/p/config/inputs.json"
	script, err = renderInitAirgap(data)
	require.NoError(t, err)
	require.Contains(t, string(script), "AIRGAP_DEPLOYMENT_ID=demo2")
	require.Contains(t, string(script), "AIRGAP_INSTALL_INPUTS_URI=s3://b/p/config/inputs.json")
}

func TestMakeStackKeys(t *testing.T) {
	require.Equal(t, stackKeys{
		Runner: "customer/install/bootstrap/runner", Init: "customer/install/bootstrap/init.sh",
		Bundle: "customer/install/bundle/bundle.tar.zst", StatePrefix: "customer/install/state",
		StackOutputs: "customer/install/stack-outputs/outputs.json",
		Inputs:       "customer/install/config/inputs.json",
		RootTemplate: "customer/install/stack/root-template.json",
	}, makeStackKeys(normalizeStackPrefix("/customer/install/"), "bundle.tar.zst"))
	require.Equal(t, "bootstrap/runner", makeStackKeys("", "bundle.tar.zst").Runner)
}

func TestLoadInstallInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inputs.yaml")
	require.NoError(t, os.WriteFile(path, []byte("domain: example.com\nreplicas: 3\nenabled: true\noverride:helm_values:my-api: |\n  replicas: 2\n"), 0o600))

	values, err := loadInstallInputs(path)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"domain": "example.com", "replicas": "3", "enabled": "true",
		config.HelmValuesOverrideInputName("my-api"): "replicas: 2\n",
	}, values)
	require.NoError(t, airgap.ValidateInputValues([]airgap.InputSpec{
		{Name: "domain", Type: "string", Bindable: true},
		{Name: "replicas", Type: "number", Bindable: true},
		{Name: "enabled", Type: "bool", Bindable: true},
		{Name: config.HelmValuesOverrideInputName("my-api"), Type: "yaml", Bindable: true},
	}, values))
}

func TestLoadInstallInputsValidationFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inputs.yaml")
	require.NoError(t, os.WriteFile(path, []byte("enabled: perhaps\nunknown: value\n"), 0o600))
	values, err := loadInstallInputs(path)
	require.NoError(t, err)
	err = airgap.ValidateInputValues([]airgap.InputSpec{{Name: "enabled", Type: "bool", Bindable: true}}, values)
	require.ErrorContains(t, err, `input "enabled" must be a boolean`)
	require.ErrorContains(t, err, `input "unknown" is not declared`)

	require.NoError(t, os.WriteFile(path, []byte("override:wrong:api: value\n"), 0o600))
	_, err = loadInstallInputs(path)
	require.ErrorContains(t, err, "invalid override input alias")
}

func TestRequiredOfflineInputs(t *testing.T) {
	missing := requiredOfflineInputs([]airgap.InputSpec{
		{Name: "domain", Type: "string", Description: "public hostname", Required: true, Bindable: true},
		{Name: "optional", Type: "string", Bindable: true},
		{Name: "defaulted", Type: "number", Required: true, Bindable: true, Default: "2"},
	})
	require.Equal(t, []string{"domain (string): public hostname"}, missing)
}

func TestInstallInputsUpload(t *testing.T) {
	upload := installInputsUpload(makeStackKeys("install/", "bundle.tar.zst").Inputs, []byte(`{"domain":"example.com"}`))
	require.Equal(t, "install/config/inputs.json", upload.Key)
	require.Equal(t, "application/json", upload.MediaType)
	body, err := upload.Open()
	require.NoError(t, err)
	defer body.Close()
	contents, err := io.ReadAll(body)
	require.NoError(t, err)
	require.JSONEq(t, `{"domain":"example.com"}`, string(contents))
}

func TestSplitImageRef(t *testing.T) {
	ref := "504178855485.dkr.ecr.us-east-1.amazonaws.com/nuon-airgap/runner:sha256-deadbeef"
	url, tag, registry, err := splitImageRef(ref)
	require.NoError(t, err)
	require.Equal(t, strings.TrimSuffix(ref, ":sha256-deadbeef"), url)
	require.Equal(t, "sha256-deadbeef", tag)
	require.Equal(t, "504178855485.dkr.ecr.us-east-1.amazonaws.com", registry)

	for _, invalid := range []string{"runner", "example.com/runner:tag", "123.dkr.ecr.us-east-1.amazonaws.com/runner"} {
		_, _, _, err := splitImageRef(invalid)
		require.Error(t, err)
	}
}
