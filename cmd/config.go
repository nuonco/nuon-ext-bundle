package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type bundleConfig struct {
	ECRRegistry     string `yaml:"ecr_registry,omitempty"`
	ECRPrefix       string `yaml:"ecr_prefix,omitempty"`
	Bucket          string `yaml:"bucket,omitempty"`
	BucketPrefix    string `yaml:"bucket_prefix,omitempty"`
	Region          string `yaml:"region,omitempty"`
	Profile         string `yaml:"profile,omitempty"`
	Image           string `yaml:"image,omitempty"`
	InstallID       string `yaml:"install_id,omitempty"`
	DeploymentID    string `yaml:"deployment_id,omitempty"`
	State           string `yaml:"state,omitempty"`
	StackName       string `yaml:"stack_name,omitempty"`
	StackTemplate   string `yaml:"stack_template,omitempty"`
	StackOutputsKey string `yaml:"stack_outputs_key,omitempty"`
}

type configPaths struct {
	active   string
	contexts string
}

func defaultConfigPaths() (configPaths, error) {
	paths := configPaths{
		active:   os.Getenv("NUON_BUNDLE_CONFIG"),
		contexts: os.Getenv("NUON_BUNDLE_CONTEXTS_DIR"),
	}
	if paths.active != "" && paths.contexts != "" {
		return paths, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return configPaths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	if paths.active == "" {
		paths.active = filepath.Join(home, ".nuon-bundle")
	}
	if paths.contexts == "" {
		paths.contexts = filepath.Join(home, ".config", "nuon-bundle", "contexts")
	}
	return paths, nil
}

func loadBundleConfig() (*bundleConfig, error) {
	paths, err := defaultConfigPaths()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(paths.active)
	if errors.Is(err, os.ErrNotExist) {
		return &bundleConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", paths.active, err)
	}
	var cfg bundleConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("decode %s: %w", paths.active, err)
	}
	return &cfg, nil
}

// Follow the active symlink so updates persist in the named context.
func saveBundleConfig(cfg *bundleConfig) error {
	paths, err := defaultConfigPaths()
	if err != nil {
		return err
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	target := paths.active
	if info, err := os.Lstat(paths.active); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := os.Readlink(paths.active)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", paths.active, err)
		}
		target = resolved
	}
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

func fallback(value, configValue string) string {
	if value != "" {
		return value
	}
	return configValue
}

func resolveStateFlags(state, profile, region *string) error {
	cfg, err := loadBundleConfig()
	if err != nil {
		return err
	}
	*state = fallback(*state, cfg.State)
	*profile = fallback(*profile, cfg.Profile)
	*region = fallback(*region, cfg.Region)
	if *state == "" {
		return fmt.Errorf("no state configured: pass --state or run `nuon-bundle stack prepare`, which saves it to the active context")
	}
	return nil
}
