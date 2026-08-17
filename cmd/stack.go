package cmd

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	nuonconfig "github.com/nuonco/nuon/pkg/config"
	"github.com/nuonco/nuon/pkg/runner/airgap"
	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

//go:embed templates/init-airgap.sh.tmpl
var initAirgapTemplate string

type stackTemplateData struct {
	Bucket, Prefix, Region, ImageURL, ImageTag, ECRRegistry, BundleKey, StatePrefix, StackOutputsURI, InstallInputsURI, RunnerID, DeploymentID string
}

type stackKeys struct {
	Runner, Init, Bundle, StatePrefix, StackOutputs, Inputs, RootTemplate string
}

type stackUpload struct {
	Key, MediaType string
	Size           int64
	Open           func() (io.ReadCloser, error)
}

type s3Uploader interface {
	Upload(context.Context, *s3.PutObjectInput, ...func(*manager.Uploader)) (*manager.UploadOutput, error)
}

func stackCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "stack", Short: "Prepare customer stack assets"}
	cmd.AddCommand(stackPrepareCmd())
	return cmd
}

func stackPrepareCmd() *cobra.Command {
	var bucket, region, profile, prefix, image, runnerID, stackName, deploymentID, inputsFile string
	var uploadBundle, dryRun, yes bool
	c := &cobra.Command{
		Use:  "prepare <bundle.tar.zst>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bcfg, err := loadBundleConfig()
			if err != nil {
				return err
			}
			bucket = fallback(bucket, bcfg.Bucket)
			prefix = fallback(prefix, bcfg.BucketPrefix)
			profile = fallback(profile, bcfg.Profile)
			region = fallback(region, bcfg.Region)
			image = fallback(image, bcfg.Image)
			deploymentID = fallback(deploymentID, bcfg.DeploymentID)
			if bucket == "" {
				return fmt.Errorf("--bucket is required (or set by the active context)")
			}
			if image == "" {
				return fmt.Errorf("--image is required (or set by the active context)")
			}
			imageURL, imageTag, registry, err := splitImageRef(image)
			if err != nil {
				return err
			}
			o, err := openArchive(cmd, args[0])
			if err != nil {
				return err
			}
			defer o.cleanup()
			if err := bundle.VerifyBlobs(o.dir); err != nil {
				return err
			}

			prefix = normalizeStackPrefix(prefix)
			basePrefix := prefix
			var frozenInstallID, deployInstallID string
			var envelope *airgap.Envelope
			if len(o.bundle.PlanEnvelope) > 0 {
				envelope, err = airgap.Parse(o.bundle.PlanEnvelope)
				if err != nil {
					return fmt.Errorf("parse bundle plan envelope: %w", err)
				}
				frozenInstallID = envelope.InstallID
			}
			var inputValues map[string]string
			var inputJSON []byte
			if inputsFile != "" {
				if envelope == nil {
					return fmt.Errorf("--inputs requires a bundle with a plan envelope")
				}
				inputValues, err = loadInstallInputs(inputsFile)
				if err != nil {
					return err
				}
				if err := airgap.ValidateInputValues(envelope.Inputs, inputValues); err != nil {
					return err
				}
				inputJSON, err = json.Marshal(inputValues)
				if err != nil {
					return fmt.Errorf("encode install inputs: %w", err)
				}
			} else if envelope != nil {
				if missing := requiredOfflineInputs(envelope.Inputs); len(missing) > 0 {
					return fmt.Errorf("--inputs is required for these offline install inputs:\n  %s", strings.Join(missing, "\n  "))
				}
			}
			if deploymentID != "" {
				if frozenInstallID == "" {
					return fmt.Errorf("--deployment-id requires a bundle with a plan envelope")
				}
				deployInstallID, err = airgap.DeploymentInstallID(frozenInstallID, deploymentID)
				if err != nil {
					return err
				}
				// Isolate repeated deployments from existing assets and state.
				prefix += deploymentID + "/"
			}
			// Include the effective install ID to prevent account-wide name collisions.
			effectiveInstallID := deployInstallID
			if effectiveInstallID == "" {
				effectiveInstallID = frozenInstallID
			}
			if runnerID == "" {
				runnerID = "airgap"
				if effectiveInstallID != "" {
					runnerID += "-" + effectiveInstallID
				}
			}
			if stackName == "" {
				stackName = "nuon-airgap"
				if effectiveInstallID != "" {
					stackName += "-" + effectiveInstallID
				}
			}
			keys := makeStackKeys(prefix, filepath.Base(args[0]))

			runnerPath := filepath.Join(o.dir, "stack-prepare-runner")
			if err := writeRunnerBinary(cmd, o.bundle, runnerPath); err != nil {
				return err
			}
			runnerInfo, err := os.Stat(runnerPath)
			if err != nil {
				return fmt.Errorf("stat runner binary: %w", err)
			}

			options := []func(*config.LoadOptions) error{}
			if profile != "" {
				options = append(options, config.WithSharedConfigProfile(profile))
			}
			if region != "" {
				options = append(options, config.WithRegion(region))
			}
			cfg, err := config.LoadDefaultConfig(cmd.Context(), options...)
			if err != nil {
				return fmt.Errorf("unable to load AWS config: %w", err)
			}
			region = cfg.Region
			if region == "" {
				return fmt.Errorf("AWS region is required via --region or AWS config")
			}

			data := stackTemplateData{
				Bucket: bucket, Prefix: prefix, Region: region, ImageURL: imageURL, ImageTag: imageTag,
				ECRRegistry: registry, BundleKey: "bundle/" + filepath.Base(args[0]),
				StatePrefix: "s3://" + bucket + "/" + keys.StatePrefix, RunnerID: runnerID,
				StackOutputsURI: "s3://" + bucket + "/" + keys.StackOutputs,
				DeploymentID:    deploymentID,
			}
			if inputsFile != "" {
				data.InstallInputsURI = "s3://" + bucket + "/" + keys.Inputs
			}
			initScript, err := renderInitAirgap(data)
			if err != nil {
				return err
			}
			rootTemplate, err := rewriteRootTemplate(o, keys, bucket, region, prefix, registry, runnerID, frozenInstallID, deployInstallID)
			if err != nil {
				return err
			}
			uploads, err := stackUploads(o, args[0], runnerPath, runnerInfo.Size(), initScript, keys, uploadBundle, frozenInstallID, deployInstallID)
			if err != nil {
				return err
			}
			if rootTemplate != nil {
				uploads = append(uploads, stackUpload{Key: keys.RootTemplate, MediaType: "application/json", Size: int64(len(rootTemplate.Contents)), Open: func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(rootTemplate.Contents)), nil
				}})
			}
			if inputsFile != "" {
				uploads = append(uploads, installInputsUpload(keys.Inputs, inputJSON))
			}
			fmt.Printf("checksum sha256:%s  %d uploads\n", o.checksum, len(uploads))
			if envelope != nil && len(envelope.Inputs) > 0 {
				printInputSummary(envelope.Inputs, inputValues)
			}
			if deployInstallID != "" {
				fmt.Printf("deployment install ID: %s (bundle install %s + deployment id %q; all physical resource names use this ID)\n", deployInstallID, frozenInstallID, deploymentID)
			}
			fmt.Printf("runner id: %s  stack name: %s\n", runnerID, stackName)
			for _, item := range uploads {
				fmt.Printf("s3://%s/%s  %s  %s\n", bucket, item.Key, humanSize(item.Size), item.MediaType)
			}
			if dryRun {
				return nil
			}
			if !yes {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("--yes is required when stdin is not a TTY")
				}
				fmt.Print("Upload these stack assets? [y/N] ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					return fmt.Errorf("stack prepare canceled")
				}
			}

			uploader := manager.NewUploader(s3.NewFromConfig(cfg, func(o *s3.Options) {
				o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			}))
			if err := uploadStackItems(cmd.Context(), uploader, bucket, uploads); err != nil {
				return err
			}
			fmt.Println("\nUploaded:")
			for _, item := range uploads {
				fmt.Printf("  %-48s %s\n", item.Key, humanSize(item.Size))
			}
			bcfg.Bucket, bcfg.BucketPrefix, bcfg.Profile, bcfg.Region = bucket, basePrefix, profile, region
			bcfg.Image, bcfg.DeploymentID = image, deploymentID
			bcfg.State = "s3://" + bucket + "/" + keys.StatePrefix
			if rootTemplate != nil {
				bcfg.StackName = stackName
				bcfg.StackTemplate = customerAssetURL(bucket, region, keys.RootTemplate)
				bcfg.StackOutputsKey = keys.StackOutputs
			}
			if err := saveBundleConfig(bcfg); err != nil {
				fmt.Fprintf(os.Stderr, "warning: unable to save context: %v\n", err)
			} else {
				fmt.Println("saved deployment settings to the active context; status/logs/results now need no flags")
			}
			fmt.Printf("\nRunnerInitScriptUrl=s3://%s/%s\n", bucket, keys.Init)
			fmt.Printf("Stack parameters for air-gapped phone-home: PhoneHomeS3Bucket=%s PhoneHomeS3Key=%s\n", bucket, keys.StackOutputs)
			fmt.Printf("The root template attaches the nuon-airgap-asset-access policy to the runner instance role (S3 read on %s, S3 write on %sstate/*, ECR on %s, CloudWatch Logs).\n", bucket, prefix, registry)
			if rootTemplate != nil {
				fmt.Printf("\nLaunch the install stack:\n\n%s\n", createStackCommand(stackName, customerAssetURL(bucket, region, keys.RootTemplate), bucket, keys, rootTemplate.RequiredParameters, region, profile))
				fmt.Printf("\nOr launch and wait with:\n\n  nuon-bundle deploy --yes\n")
				if len(rootTemplate.RequiredParameters) > 0 {
					fmt.Printf("\nReplace the <placeholder> values; those parameters have no template default.\n")
				}
				fmt.Printf("\nFollow the run once the stack is up:\n\n  nuon-bundle status --follow\n")
			} else {
				fmt.Println("\nBundle has no root stack template; launch the install stack from a connected quick-link.")
			}
			return nil
		},
	}
	c.Flags().StringVar(&bucket, "bucket", "", "customer S3 bucket name")
	c.Flags().StringVar(&region, "region", "", "AWS region (defaults to AWS config)")
	c.Flags().StringVar(&profile, "profile", "", "AWS profile")
	c.Flags().StringVar(&prefix, "prefix", "", "key prefix inside the bucket")
	c.Flags().StringVar(&image, "image", "", "full customer ECR runner image reference")
	c.Flags().StringVar(&runnerID, "runner-id", "", "runner label used for CloudWatch logs (defaults to airgap-<install-id>)")
	c.Flags().StringVar(&stackName, "stack-name", "", "CloudFormation stack name used in the printed create-stack command (defaults to nuon-airgap-<install-id>)")
	c.Flags().StringVar(&deploymentID, "deployment-id", "", "short deployment identifier (1-8 lowercase letters or digits) spliced into the bundle's frozen install ID; makes a second deployment of the same bundle in one account collision-free (scopes S3 keys and rewrites stack templates, and the runner rewrites plans to match)")
	c.Flags().StringVar(&inputsFile, "inputs", "", "YAML or JSON file containing a flat map of install input names to scalar values")
	c.Flags().BoolVar(&uploadBundle, "upload-bundle", true, "upload the bundle archive")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without uploading")
	c.Flags().BoolVar(&yes, "yes", false, "skip confirmation")

	return c
}

func normalizeStackPrefix(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

func makeStackKeys(prefix, archiveName string) stackKeys {
	return stackKeys{
		Runner:      prefix + "bootstrap/runner",
		Init:        prefix + "bootstrap/init.sh",
		Bundle:      prefix + "bundle/" + archiveName,
		StatePrefix: prefix + "state",
		// The phone-home Lambda writes this key before the runner late-binds plans.
		StackOutputs: prefix + "stack-outputs/outputs.json",
		Inputs:       prefix + "config/inputs.json",
		RootTemplate: prefix + "stack/root-template.json",
	}
}

func loadInstallInputs(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read install inputs %s: %w", path, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode install inputs %s: %w", path, err)
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("decode install inputs %s: expected a map of input names to scalar values", path)
	}
	values := make(map[string]string, len(document.Content[0].Content)/2)
	entries := document.Content[0].Content
	for i := 0; i < len(entries); i += 2 {
		key, value := entries[i], entries[i+1]
		if key.Kind != yaml.ScalarNode || value.Kind != yaml.ScalarNode || value.Tag == "!!null" {
			return nil, fmt.Errorf("decode install inputs %s: input values must be scalars", path)
		}
		name, err := installInputName(key.Value)
		if err != nil {
			return nil, err
		}
		if _, exists := values[name]; exists {
			return nil, fmt.Errorf("install input %q is specified more than once", key.Value)
		}
		values[name] = value.Value
	}
	return values, nil
}

func installInputName(name string) (string, error) {
	const prefix = "override:"
	if !strings.HasPrefix(name, prefix) {
		return name, nil
	}
	rest := strings.TrimPrefix(name, prefix)
	for _, kind := range []nuonconfig.ComponentOverrideKind{nuonconfig.ComponentOverrideKindHelmValues, nuonconfig.ComponentOverrideKindTFVars, nuonconfig.ComponentOverrideKindEnabled} {
		kindPrefix := string(kind) + ":"
		if !strings.HasPrefix(rest, kindPrefix) {
			continue
		}
		component := strings.TrimPrefix(rest, kindPrefix)
		if component == "" {
			break
		}
		switch kind {
		case nuonconfig.ComponentOverrideKindHelmValues:
			return nuonconfig.HelmValuesOverrideInputName(component), nil
		case nuonconfig.ComponentOverrideKindTFVars:
			return nuonconfig.TFVarsOverrideInputName(component), nil
		case nuonconfig.ComponentOverrideKindEnabled:
			return nuonconfig.EnabledOverrideInputName(component), nil
		}
	}
	return "", fmt.Errorf("invalid override input alias %q; expected override:<helm_values|tf_vars|enabled>:<component>", name)
}

func requiredOfflineInputs(specs []airgap.InputSpec) []string {
	var missing []string
	for _, spec := range specs {
		if spec.Required && spec.Bindable && !spec.Secret && spec.Default == "" {
			missing = append(missing, fmt.Sprintf("%s (%s): %s", displayInputName(spec.Name), spec.Type, spec.Description))
		}
	}
	return missing
}

func printInputSummary(specs []airgap.InputSpec, provided map[string]string) {
	fmt.Println("\nINPUTS")
	for _, spec := range specs {
		if !spec.Bindable || spec.Secret {
			continue
		}
		markers := []string{"editable"}
		if spec.Required {
			markers = append(markers, "required")
		}
		if spec.Default != "" {
			markers = append(markers, "default="+spec.Default)
		}
		if _, ok := provided[spec.Name]; ok {
			markers = append(markers, "provided")
		}
		fmt.Printf("  %s (%s)  %s\n", displayInputName(spec.Name), spec.Type, strings.Join(markers, ", "))
	}
}

func installInputsUpload(key string, contents []byte) stackUpload {
	return stackUpload{Key: key, MediaType: "application/json", Size: int64(len(contents)), Open: func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(contents)), nil
	}}
}

func splitImageRef(ref string) (imageURL, imageTag, registry string, err error) {
	slash := strings.IndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if slash <= 0 || colon <= slash+1 || colon == len(ref)-1 {
		return "", "", "", fmt.Errorf("--image must be a full tagged ECR image reference")
	}
	imageURL, imageTag, registry = ref[:colon], ref[colon+1:], ref[:slash]
	if !strings.Contains(registry, ".dkr.ecr.") {
		return "", "", "", fmt.Errorf("--image must use an ECR registry")
	}
	return imageURL, imageTag, registry, nil
}

func renderInitAirgap(data stackTemplateData) ([]byte, error) {
	tmpl, err := template.New("init-airgap").Option("missingkey=error").Parse(initAirgapTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse airgap init template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("render airgap init template: %w", err)
	}
	return out.Bytes(), nil
}

func stackUploads(o *opened, archivePath, runnerPath string, runnerSize int64, initScript []byte, keys stackKeys, includeBundle bool, frozenInstallID, deployInstallID string) ([]stackUpload, error) {
	openFile := func(path string) func() (io.ReadCloser, error) {
		return func() (io.ReadCloser, error) { return os.Open(path) }
	}
	uploads := []stackUpload{
		{Key: keys.Runner, MediaType: "application/octet-stream", Size: runnerSize, Open: openFile(runnerPath)},
		{Key: keys.Init, MediaType: "text/x-shellscript", Size: int64(len(initScript)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(initScript)), nil }},
	}
	for _, asset := range o.bundle.Manifest.StackAssets {
		layer, err := stackAssetContentLayer(o.dir, asset)
		if err != nil {
			return nil, err
		}
		upload := stackUpload{Key: strings.TrimSuffix(keys.StatePrefix, "state") + "stack/" + asset.Role, MediaType: layer.MediaType, Size: layer.Size, Open: openFile(blobPath(o.dir, layer.Digest.String()))}
		if asset.Role == "runner" || deployInstallID != "" {
			raw, err := os.ReadFile(blobPath(o.dir, layer.Digest.String()))
			if err != nil {
				return nil, fmt.Errorf("read stack asset %s blob: %w", asset.Role, err)
			}
			if asset.Role == "runner" {
				raw, err = rewriteRunnerStackAsset(raw)
				if err != nil {
					return nil, err
				}
			}
			raw = substituteInstallID(raw, frozenInstallID, deployInstallID)
			rewritten := raw
			upload.Size = int64(len(rewritten))
			upload.Open = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rewritten)), nil }
		}
		uploads = append(uploads, upload)
	}
	if includeBundle {
		uploads = append(uploads, stackUpload{Key: keys.Bundle, MediaType: "application/zstd", Size: o.size, Open: openFile(archivePath)})
	}
	return uploads, nil
}

func uploadStackItems(ctx context.Context, uploader s3Uploader, bucket string, uploads []stackUpload) error {
	for _, item := range uploads {
		body, err := item.Open()
		if err != nil {
			return fmt.Errorf("open %s: %w", item.Key, err)
		}
		_, uploadErr := uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(item.Key), Body: body,
			ContentType: aws.String(item.MediaType),
		})
		closeErr := body.Close()
		if uploadErr != nil {
			return fmt.Errorf("upload s3://%s/%s: %w", bucket, item.Key, uploadErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", item.Key, closeErr)
		}
	}
	return nil
}
