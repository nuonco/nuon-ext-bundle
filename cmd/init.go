package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

func initCmd() *cobra.Command {
	var name, file string
	c := &cobra.Command{
		Use:   "init [--file <config.yaml>] [--name <context>]",
		Short: "set up the deployment context used by all other commands",
		Example: `  # interactive form
  nuon-bundle init

  # file-based: write the settings once, then create the context from them
  cat > acme.yaml <<'EOF'
  ecr_registry: 111122223333.dkr.ecr.us-east-1.amazonaws.com
  ecr_prefix: acme
  bucket: acme-nuon-install
  bucket_prefix: install/
  region: us-east-1
  profile: customer-admin
  EOF
  nuon-bundle init --name acme --file acme.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := defaultConfigPaths()
			if err != nil {
				return err
			}
			cfg := &bundleConfig{}
			if file == "" {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("stdin is not a terminal; pass --file <config.yaml> (see `nuon-bundle init -h`)")
				}
				if err := runInitForm(cmd.Context(), paths, cfg, &name); err != nil {
					return err
				}
			} else {
				if cfg, err = loadConfigFile(file); err != nil {
					return err
				}
			}
			if name != "" {
				target, err := contextPath(paths, name)
				if err != nil {
					return err
				}
				if _, err := os.Stat(target); err == nil {
					return fmt.Errorf("context %q already exists; switch to it with `nuon-bundle ctx %s` or delete it first", name, name)
				}
				raw, err := yaml.Marshal(cfg)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(paths.contexts, 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(target, raw, 0o600); err != nil {
					return err
				}
				if info, err := os.Lstat(paths.active); err == nil {
					if info.Mode()&os.ModeSymlink == 0 {
						return fmt.Errorf("%s is a regular file; save it first with `nuon-bundle ctx -s <name>`", paths.active)
					}
					if previous, err := currentContext(paths); err == nil {
						_ = os.WriteFile(filepath.Join(paths.contexts, ".previous"), []byte(previous), 0o600)
					}
					if err := os.Remove(paths.active); err != nil {
						return err
					}
				}
				if err := os.Symlink(target, paths.active); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "created and activated context %q\n\n", name)
			} else {
				if err := saveBundleConfig(cfg); err != nil {
					return err
				}
				if active, err := currentContext(paths); err == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "updated context %q\n\n", active)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "updated %s\n\n", paths.active)
				}
			}
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(raw)
			return err
		},
	}
	c.Flags().StringVar(&name, "name", "", "create and activate a named context instead of updating the active one")
	c.Flags().StringVarP(&file, "file", "f", "", "YAML file with context settings (same keys as the context file)")
	return c
}

// Reject unknown keys so misspelled settings do not silently disappear.
func loadConfigFile(path string) (*bundleConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var cfg bundleConfig
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &cfg, nil
}

func runInitForm(ctx context.Context, paths configPaths, out *bundleConfig, name *string) error {
	existing, err := loadBundleConfig()
	if err != nil {
		return err
	}
	*out = *existing
	activeName, _ := currentContext(paths)
	if *name == "" {
		*name = activeName
	}

	required := func(field string) func(string) error {
		return func(v string) error {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("%s is required", field)
			}
			return nil
		}
	}

	identity := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Deployment name").
				Description("Names this installation, e.g. acme-prod. Reusing the active name updates it; a new name creates a fresh context.").
				Validate(func(v string) error {
					_, err := contextPath(paths, strings.TrimSpace(v))
					return err
				}).
				Value(name),
			huh.NewInput().
				Title("AWS profile").
				Description("Profile from ~/.aws/config with access to this account. Leave empty for the default credential chain.").
				Value(&out.Profile),
			huh.NewInput().
				Title("AWS region").
				Description("Region the runner stack and ECR live in, e.g. us-east-1.").
				Validate(required("AWS region")).
				Value(&out.Region),
		),
	)
	if err := identity.Run(); err != nil {
		return err
	}

	if out.ECRRegistry == "" {
		out.ECRRegistry = suggestRegistry(ctx, out.Profile, out.Region)
	}

	destinations := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("ECR registry").
				Description("Registry the bundle images are pushed to. Pre-filled from your AWS identity when possible.").
				Validate(required("ECR registry")).
				Value(&out.ECRRegistry),
			huh.NewInput().
				Title("ECR repository prefix").
				Description("Optional prefix for all pushed repositories, e.g. acme.").
				Value(&out.ECRPrefix),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("S3 bucket").
				Description("Bucket holding the install's plan, state, and stack outputs.").
				Validate(required("S3 bucket")).
				Value(&out.Bucket),
			huh.NewInput().
				Title("S3 key prefix").
				Description("Optional prefix inside the bucket, e.g. install/.").
				Value(&out.BucketPrefix),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Vendor install ID").
				Description("Optional inl... ID issued by the vendor for this delivery (from `nuon apps bundles installs create`). Lets the vendor correlate this installation with their records.").
				Value(&out.InstallID),
			huh.NewInput().
				Title("Deployment ID").
				Description("Leave empty for the first install. Set only when installing the same bundle a second time in this account.").
				Value(&out.DeploymentID),
		),
	)
	if err := destinations.Run(); err != nil {
		return err
	}

	if *name == activeName && activeName != "" {
		*name = ""
	}
	return nil
}

func suggestRegistry(ctx context.Context, profile, region string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	options := []func(*config.LoadOptions) error{}
	if profile != "" {
		options = append(options, config.WithSharedConfigProfile(profile))
	}
	if region != "" {
		options = append(options, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil || cfg.Region == "" {
		return ""
	}
	identity, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || identity.Account == nil {
		return ""
	}
	return fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com", *identity.Account, cfg.Region)
}
