package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

func writeBlob(t *testing.T, dir string, data []byte) string {
	t.Helper()
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	blobDir := filepath.Join(dir, "blobs", "sha256")
	require.NoError(t, os.MkdirAll(blobDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(blobDir, digest), data, 0o600))
	return "sha256:" + digest
}

// Match the export pipeline's single-layer OCI wrapper layout.
func writePackedAsset(t *testing.T, dir, layerMediaType string, data []byte) (manifestDigest string, manifestSize int64) {
	t.Helper()
	layerDigest := writeBlob(t, dir, data)
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"layers": []any{
			map[string]any{"mediaType": layerMediaType, "digest": layerDigest, "size": len(data)},
		},
	}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	return writeBlob(t, dir, raw), int64(len(raw))
}

func TestRewriteRootTemplate(t *testing.T) {
	dir := t.TempDir()
	rootDoc := map[string]any{
		"Parameters": map[string]any{
			"PhoneHomeS3Bucket":                 map[string]any{"Type": "String", "Default": ""},
			"CustomerVPCCIDR":                   map[string]any{"Type": "String"},
			"EnableBreakglassSandboxBreakGlass": map[string]any{"Type": "String", "Default": false},
			"EnableRunnerProvision":             map[string]any{"Type": "String", "Default": true},
		},
		"Resources": map[string]any{
			"VPC": map[string]any{
				"Properties": map[string]any{
					"TemplateURL": map[string]any{"Fn::Join": []any{"", []any{"https://vendor.s3.amazonaws.com/vpc.yaml"}}},
				},
			},
			"RunnerAutoScalingGroup": map[string]any{
				"Properties": map[string]any{
					"TemplateURL": map[string]any{"Fn::Join": []any{"", []any{"https://vendor.s3.amazonaws.com/runner.yaml"}}},
					"Parameters": map[string]any{
						"RunnerInitScriptUrl": "https://raw.githubusercontent.com/nuonco/runner/refs/heads/main/scripts/aws/init.sh",
						"RunnerId":            "connected-runner-id",
					},
				},
			},
			"RunnerCloudWatchLogGroup": map[string]any{
				"Properties": map[string]any{"LogGroupName": "runner-connected-runner-id"},
			},
			"MaintenanceRole": map[string]any{
				"Properties": map[string]any{
					"Policies": []any{"arn:aws:secretsmanager:__NUON_AIRGAP_STACK_region__::secret:rds!*"},
				},
			},
		},
	}
	rootRaw, err := json.Marshal(rootDoc)
	require.NoError(t, err)
	rootDigest, rootSize := writePackedAsset(t, dir, "application/json", rootRaw)

	o := &opened{
		dir: dir,
		bundle: &bundle.Bundle{Manifest: bundle.LogicalManifest{StackAssets: []bundle.StackAsset{
			{Role: "root", SourceURL: "https://vendor.s3.amazonaws.com/root.json", Digest: rootDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: rootSize},
			{Role: "vpc", SourceURL: "https://vendor.s3.amazonaws.com/vpc.yaml"},
			{Role: "runner", SourceURL: "https://vendor.s3.amazonaws.com/runner.yaml"},
			{Role: "init_script", SourceURL: "https://raw.githubusercontent.com/nuonco/runner/refs/heads/main/scripts/aws/init.sh#default"},
		}}},
	}
	keys := makeStackKeys("install/", "bundle.tar.zst")
	prepared, err := rewriteRootTemplate(o, keys, "customer-bucket", "us-east-1", "install/", "111122223333.dkr.ecr.us-east-1.amazonaws.com", "airgap", "", "")
	require.NoError(t, err)
	require.NotNil(t, prepared)

	text := string(prepared.Contents)
	require.Contains(t, text, "https://customer-bucket.s3.us-east-1.amazonaws.com/install/stack/vpc")
	require.Contains(t, text, "https://customer-bucket.s3.us-east-1.amazonaws.com/install/stack/runner")
	require.Contains(t, text, "s3://customer-bucket/install/bootstrap/init.sh")
	require.NotContains(t, text, "vendor.s3.amazonaws.com/vpc.yaml")
	require.NotContains(t, text, "vendor.s3.amazonaws.com/runner.yaml")
	require.NotContains(t, text, "githubusercontent")
	require.NotContains(t, text, "connected-runner-id")
	require.Contains(t, text, `"RunnerId":"airgap"`)
	require.Contains(t, text, "runner-airgap")
	require.NotContains(t, text, stackRegionPlaceholder)
	require.Contains(t, text, "arn:aws:secretsmanager:us-east-1::secret:rds!*")

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prepared.Contents, &doc))
	policy, ok := doc["Resources"].(map[string]any)["NuonAirgapAssetAccess"].(map[string]any)
	require.True(t, ok, "rewritten template must inject the airgap asset-access policy")
	require.Equal(t, "AWS::IAM::Policy", policy["Type"])
	require.Contains(t, text, "arn:aws:s3:::customer-bucket/install/*")

	params := doc["Parameters"].(map[string]any)
	breakGlass := params["EnableBreakglassSandboxBreakGlass"].(map[string]any)
	require.Equal(t, "true", breakGlass["Default"], "break-glass parameters must default to enabled: the compiled envelope bakes their role ARN tokens in unconditionally")
	provision := params["EnableRunnerProvision"].(map[string]any)
	require.Equal(t, true, provision["Default"], "non-break-glass Enable parameters must keep their defaults")
	require.Contains(t, text, "arn:aws:s3:::customer-bucket/install/state/*")
	require.Contains(t, text, "arn:aws:ecr:us-east-1:111122223333:repository/*")
	require.Contains(t, text, "arn:aws:logs:*:*:log-group:runner-airgap*")

	require.Equal(t, []string{"CustomerVPCCIDR"}, prepared.RequiredParameters)
}

func TestRewriteRootTemplateMissingRootAsset(t *testing.T) {
	o := &opened{dir: t.TempDir(), bundle: &bundle.Bundle{Manifest: bundle.LogicalManifest{StackAssets: []bundle.StackAsset{{Role: "vpc", SourceURL: "https://vendor/vpc.yaml"}}}}}
	prepared, err := rewriteRootTemplate(o, makeStackKeys("", "b.tar.zst"), "bucket", "us-east-1", "", "111122223333.dkr.ecr.us-east-1.amazonaws.com", "airgap", "", "")
	require.NoError(t, err)
	require.Nil(t, prepared)
}

func TestRewriteRunnerStackAsset(t *testing.T) {
	raw := []byte("        UserData:\n          Fn::Base64:\n            Fn::Join:\n            - \"\\n\"\n            - - \"#!/bin/bash\"\n              - Fn::Sub: curl ${RunnerInitScriptUrl} | bash\n")
	rewritten, err := rewriteRunnerStackAsset(raw)
	require.NoError(t, err)
	require.NotContains(t, string(rewritten), "curl ${RunnerInitScriptUrl}")
	require.Contains(t, string(rewritten), "aws s3 cp ${RunnerInitScriptUrl} /tmp/nuon-init.sh")
	require.Contains(t, string(rewritten), "bash /tmp/nuon-init.sh")

	_, err = rewriteRunnerStackAsset([]byte("no fetch command here"))
	require.Error(t, err)
}

func TestSubstituteInstallID(t *testing.T) {
	raw := []byte(`{"role":"inlqwvpl9h932fzsjpyqedzn93-provision","log":"runner-inlqwvpl9h932fzsjpyqedzn93"}`)
	require.Equal(t, raw, substituteInstallID(raw, "", ""))
	require.Equal(t, raw, substituteInstallID(raw, "inlqwvpl9h932fzsjpyqedzn93", ""))
	require.Equal(t, raw, substituteInstallID(raw, "", "inlqwvpl9h932fzsjpyq-demo2"))

	rewritten := substituteInstallID(raw, "inlqwvpl9h932fzsjpyqedzn93", "inlqwvpl9h932fzsjpyq-demo2")
	require.NotContains(t, string(rewritten), "inlqwvpl9h932fzsjpyqedzn93")
	require.Contains(t, string(rewritten), `"role":"inlqwvpl9h932fzsjpyq-demo2-provision"`)
	require.Contains(t, string(rewritten), `"log":"runner-inlqwvpl9h932fzsjpyq-demo2"`)
}

func TestRewriteRootTemplateAppliesDeploymentInstallID(t *testing.T) {
	const frozen = "inlqwvpl9h932fzsjpyqedzn93"
	const derived = "inlqwvpl9h932fzsjpyq-demo2"
	dir := t.TempDir()
	rootDoc := map[string]any{
		"Resources": map[string]any{
			"RunnerAutoScalingGroup": map[string]any{
				"Properties": map[string]any{
					"TemplateURL": "https://vendor.s3.amazonaws.com/runner.yaml",
					"Parameters":  map[string]any{"RoleName": frozen + "-runner", "LogGroup": "runner-" + frozen},
				},
			},
		},
	}
	rootRaw, err := json.Marshal(rootDoc)
	require.NoError(t, err)
	rootDigest, rootSize := writePackedAsset(t, dir, "application/json", rootRaw)
	o := &opened{
		dir: dir,
		bundle: &bundle.Bundle{Manifest: bundle.LogicalManifest{StackAssets: []bundle.StackAsset{
			{Role: "root", SourceURL: "https://vendor.s3.amazonaws.com/root.json", Digest: rootDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: rootSize},
			{Role: "runner", SourceURL: "https://vendor.s3.amazonaws.com/runner.yaml"},
		}}},
	}
	prepared, err := rewriteRootTemplate(o, makeStackKeys("p/", "b.tar.zst"), "bucket", "us-east-1", "p/", "111122223333.dkr.ecr.us-east-1.amazonaws.com", "airgap", frozen, derived)
	require.NoError(t, err)
	require.NotNil(t, prepared)
	text := string(prepared.Contents)
	require.NotContains(t, text, frozen)
	require.Contains(t, text, derived+"-runner")
	require.Contains(t, text, "runner-"+derived)
}

func TestStackUploadsRewritesInstallID(t *testing.T) {
	const frozen = "inlqwvpl9h932fzsjpyqedzn93"
	const derived = "inlqwvpl9h932fzsjpyq-demo2"
	dir := t.TempDir()
	vpcRaw := []byte("RoleName: " + frozen + "-vpc-flow-logs\n")
	vpcDigest, vpcSize := writePackedAsset(t, dir, "application/x-yaml", vpcRaw)
	runnerRaw := []byte("UserData: curl ${RunnerInitScriptUrl} | bash\nLogGroup: runner-" + frozen + "\n")
	runnerDigest, runnerSize := writePackedAsset(t, dir, "application/x-yaml", runnerRaw)
	o := &opened{
		dir: dir,
		bundle: &bundle.Bundle{Manifest: bundle.LogicalManifest{StackAssets: []bundle.StackAsset{
			{Role: "vpc", SourceURL: "https://vendor/vpc.yaml", Digest: vpcDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: vpcSize},
			{Role: "runner", SourceURL: "https://vendor/runner.yaml", Digest: runnerDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: runnerSize},
		}}},
	}
	runnerBin := filepath.Join(dir, "runner-bin")
	require.NoError(t, os.WriteFile(runnerBin, []byte("bin"), 0o600))
	uploads, err := stackUploads(o, "b.tar.zst", runnerBin, 3, []byte("init"), makeStackKeys("p/", "b.tar.zst"), false, frozen, derived)
	require.NoError(t, err)

	contentsByKey := map[string]string{}
	for _, item := range uploads {
		body, err := item.Open()
		require.NoError(t, err)
		raw, err := io.ReadAll(body)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		require.Equal(t, item.Size, int64(len(raw)))
		contentsByKey[item.Key] = string(raw)
	}
	vpc := contentsByKey["p/stack/vpc"]
	require.NotContains(t, vpc, frozen)
	require.Contains(t, vpc, derived+"-vpc-flow-logs")
	runner := contentsByKey["p/stack/runner"]
	require.NotContains(t, runner, frozen)
	require.Contains(t, runner, "runner-"+derived)
	require.Contains(t, runner, "aws s3 cp ${RunnerInitScriptUrl} /tmp/nuon-init.sh")
}

func TestCreateStackCommand(t *testing.T) {
	keys := makeStackKeys("install/", "bundle.tar.zst")
	cmd := createStackCommand("nuon-airgap", "https://customer-bucket.s3.us-east-1.amazonaws.com/install/stack/root-template.json", "customer-bucket", keys, []string{"CustomerVPCCIDR"}, "us-east-1", "customer-admin")
	require.Contains(t, cmd, "--stack-name nuon-airgap")
	require.Contains(t, cmd, "--region us-east-1")
	require.Contains(t, cmd, "--profile customer-admin")
	require.Contains(t, cmd, "--template-url https://customer-bucket.s3.us-east-1.amazonaws.com/install/stack/root-template.json")
	require.Contains(t, cmd, "CAPABILITY_NAMED_IAM")
	require.Contains(t, cmd, "ParameterKey=PhoneHomeS3Bucket,ParameterValue=customer-bucket")
	require.Contains(t, cmd, "ParameterKey=PhoneHomeS3Key,ParameterValue=install/stack-outputs/outputs.json")
	require.Contains(t, cmd, "ParameterKey=CustomerVPCCIDR,ParameterValue=<CustomerVPCCIDR>")
}
