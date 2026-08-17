package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

const rootStackAssetRole = "root"

type preparedRootTemplate struct {
	Contents           []byte
	RequiredParameters []string
}

func findStackAsset(manifest bundle.LogicalManifest, role string) (bundle.StackAsset, bool) {
	for _, asset := range manifest.StackAssets {
		if asset.Role == role {
			return asset, true
		}
	}
	return bundle.StackAsset{}, false
}

func blobPath(dir string, digest string) string {
	return filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

// The logical manifest digest identifies an OCI wrapper, not the template layer.
func stackAssetContentLayer(dir string, asset bundle.StackAsset) (ocispec.Descriptor, error) {
	raw, err := os.ReadFile(blobPath(dir, asset.Digest))
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("read stack asset %s manifest: %w", asset.Role, err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("decode stack asset %s manifest: %w", asset.Role, err)
	}
	if len(manifest.Layers) != 1 {
		return ocispec.Descriptor{}, fmt.Errorf("stack asset %s manifest has %d layers, expected 1", asset.Role, len(manifest.Layers))
	}
	return manifest.Layers[0], nil
}

// CloudFormation nested-stack TemplateURLs require an HTTPS S3 URL.
func customerAssetURL(bucket, region, key string) string {
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", bucket, region, key)
}

const connectedInitFetch = "curl ${RunnerInitScriptUrl} | bash"

// Retry because first boot can race attachment of the instance role's asset policy.
const airgapInitFetch = "for i in $(seq 1 60); do aws s3 cp ${RunnerInitScriptUrl} /tmp/nuon-init.sh && break; sleep 10; done; bash /tmp/nuon-init.sh"

func rewriteRunnerStackAsset(raw []byte) ([]byte, error) {
	if !bytes.Contains(raw, []byte(connectedInitFetch)) {
		return nil, fmt.Errorf("runner stack template does not contain the expected init fetch command %q", connectedInitFetch)
	}
	return bytes.ReplaceAll(raw, []byte(connectedInitFetch), []byte(airgapInitFetch)), nil
}

func airgapAssetAccessPolicy(bucket, prefix, registry, runnerID string) (map[string]any, error) {
	account, _, _ := strings.Cut(registry, ".")
	ecrRegion := regionFromRegistry(registry)
	if account == "" || ecrRegion == "" {
		return nil, fmt.Errorf("unable to derive account and region from ECR registry %q", registry)
	}
	statement := []any{
		map[string]any{"Effect": "Allow", "Action": []any{"s3:GetObject"}, "Resource": fmt.Sprintf("arn:aws:s3:::%s/%s*", bucket, prefix)},
		map[string]any{"Effect": "Allow", "Action": []any{"s3:ListBucket"}, "Resource": "arn:aws:s3:::" + bucket},
		map[string]any{"Effect": "Allow", "Action": []any{"s3:PutObject"}, "Resource": fmt.Sprintf("arn:aws:s3:::%s/%sstate/*", bucket, prefix)},
		map[string]any{"Effect": "Allow", "Action": []any{"ecr:GetAuthorizationToken"}, "Resource": "*"},
		map[string]any{"Effect": "Allow", "Action": []any{
			"ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer",
			"ecr:DescribeRepositories", "ecr:DescribeImages", "ecr:ListImages",
			"ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload",
			"ecr:PutImage", "ecr:CreateRepository", "ecr:TagResource",
		}, "Resource": fmt.Sprintf("arn:aws:ecr:%s:%s:repository/*", ecrRegion, account)},
		map[string]any{"Effect": "Allow", "Action": []any{
			"logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams",
		}, "Resource": fmt.Sprintf("arn:aws:logs:*:*:log-group:runner-%s*", runnerID)},
	}
	return map[string]any{
		"Type": "AWS::IAM::Policy",
		"Properties": map[string]any{
			"PolicyName": "nuon-airgap-asset-access",
			"Roles":      []any{map[string]any{"Fn::GetAtt": []any{"RunnerAutoScalingGroup", "Outputs.RunnerInstanceRole"}}},
			"PolicyDocument": map[string]any{
				"Version":   "2012-10-17",
				"Statement": statement,
			},
		},
	}, nil
}

func rewriteRootTemplate(o *opened, keys stackKeys, bucket, region, prefix, registry, runnerID, frozenInstallID, deployInstallID string) (*preparedRootTemplate, error) {
	rootAsset, ok := findStackAsset(o.bundle.Manifest, rootStackAssetRole)
	if !ok {
		return nil, nil
	}
	layer, err := stackAssetContentLayer(o.dir, rootAsset)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(blobPath(o.dir, layer.Digest.String()))
	if err != nil {
		return nil, fmt.Errorf("read root stack template blob: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode root stack template: %w", err)
	}
	frozenRunnerID := rootTemplateRunnerID(doc)
	replacements := map[string]string{}
	for _, asset := range o.bundle.Manifest.StackAssets {
		if asset.Role == rootStackAssetRole || asset.SourceURL == "" {
			continue
		}
		target := customerAssetURL(bucket, region, prefix+"stack/"+asset.Role)
		if asset.Role == "init_script" {
			// Private bucket assets require instance-role authentication.
			target = "s3://" + bucket + "/" + keys.Init
		}
		replacements[asset.SourceURL] = target
		// Renderers may omit the fragment recorded in the manifest.
		if base, _, found := strings.Cut(asset.SourceURL, "#"); found {
			replacements[base] = target
		}
	}
	rewriteStringValues(doc, replacements)
	enableBreakGlassParameters(doc)
	resources, ok := doc["Resources"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("root stack template has no Resources section")
	}
	if _, ok := resources["RunnerAutoScalingGroup"]; !ok {
		return nil, fmt.Errorf("root stack template has no RunnerAutoScalingGroup resource to attach the airgap asset-access policy to")
	}
	policy, err := airgapAssetAccessPolicy(bucket, prefix, registry, runnerID)
	if err != nil {
		return nil, err
	}
	resources["NuonAirgapAssetAccess"] = policy
	contents, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode rewritten root stack template: %w", err)
	}
	if frozenRunnerID != "" && runnerID != "" {
		contents = bytes.ReplaceAll(contents, []byte(frozenRunnerID), []byte(runnerID))
	}
	contents = substituteInstallID(contents, frozenInstallID, deployInstallID)
	// Compiled templates carry the publish-time region placeholder (from
	// `.nuon.cloud_account.aws.region` or stack-output references in the
	// permissions/break-glass configs); CloudFormation never resolves it, so
	// splice in the deploy region here.
	contents = bytes.ReplaceAll(contents, []byte(stackRegionPlaceholder), []byte(region))
	return &preparedRootTemplate{Contents: contents, RequiredParameters: requiredTemplateParameters(doc)}, nil
}

const stackRegionPlaceholder = "__NUON_AIRGAP_STACK_region__"

// enableBreakGlassParameters defaults every EnableBreakglass* stack parameter
// to "true". The compiled plan envelope seeds break-glass role ARN tokens
// unconditionally, so vendor templates guarded by
// `len .nuon.install_stack.outputs.break_glass_role_arns > 0` already rendered
// with those tokens baked in; the stack must create the roles or the runner
// can never late-bind them.
func enableBreakGlassParameters(doc map[string]any) {
	parameters, _ := doc["Parameters"].(map[string]any)
	for name, value := range parameters {
		if !strings.HasPrefix(name, "EnableBreakglass") {
			continue
		}
		if spec, ok := value.(map[string]any); ok {
			spec["Default"] = "true"
		}
	}
}

func rootTemplateRunnerID(doc map[string]any) string {
	resources, _ := doc["Resources"].(map[string]any)
	runner, _ := resources["RunnerAutoScalingGroup"].(map[string]any)
	properties, _ := runner["Properties"].(map[string]any)
	parameters, _ := properties["Parameters"].(map[string]any)
	runnerID, _ := parameters["RunnerId"].(string)
	return runnerID
}

func substituteInstallID(raw []byte, frozenInstallID, deployInstallID string) []byte {
	if frozenInstallID == "" || deployInstallID == "" || frozenInstallID == deployInstallID {
		return raw
	}
	return bytes.ReplaceAll(raw, []byte(frozenInstallID), []byte(deployInstallID))
}

func rewriteStringValues(value any, replacements map[string]string) any {
	switch node := value.(type) {
	case string:
		if replacement, ok := replacements[node]; ok {
			return replacement
		}
		return node
	case map[string]any:
		for key, child := range node {
			node[key] = rewriteStringValues(child, replacements)
		}
		return node
	case []any:
		for i, child := range node {
			node[i] = rewriteStringValues(child, replacements)
		}
		return node
	default:
		return value
	}
}

func requiredTemplateParameters(doc map[string]any) []string {
	parameters, _ := doc["Parameters"].(map[string]any)
	var required []string
	for name, value := range parameters {
		spec, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if _, hasDefault := spec["Default"]; !hasDefault {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	return required
}

func createStackCommand(stackName, templateURL, bucket string, keys stackKeys, requiredParams []string, region, profile string) string {
	var b strings.Builder
	b.WriteString("aws cloudformation create-stack \\\n")
	fmt.Fprintf(&b, "  --stack-name %s \\\n", stackName)
	fmt.Fprintf(&b, "  --region %s \\\n", region)
	if profile != "" {
		fmt.Fprintf(&b, "  --profile %s \\\n", profile)
	}
	fmt.Fprintf(&b, "  --template-url %s \\\n", templateURL)
	b.WriteString("  --capabilities CAPABILITY_NAMED_IAM CAPABILITY_AUTO_EXPAND \\\n")
	b.WriteString("  --parameters \\\n")
	fmt.Fprintf(&b, "    ParameterKey=PhoneHomeS3Bucket,ParameterValue=%s \\\n", bucket)
	fmt.Fprintf(&b, "    ParameterKey=PhoneHomeS3Key,ParameterValue=%s", keys.StackOutputs)
	for _, name := range requiredParams {
		fmt.Fprintf(&b, " \\\n    ParameterKey=%s,ParameterValue=<%s>", name, name)
	}
	return b.String()
}
