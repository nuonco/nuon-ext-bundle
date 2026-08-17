package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func deployCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "deploy",
		Short: "launch the prepared install stack and wait for it to finish",
		RunE: func(cmd *cobra.Command, _ []string) error {
			bcfg, err := loadBundleConfig()
			if err != nil {
				return err
			}
			if bcfg.StackName == "" || bcfg.StackTemplate == "" || bcfg.Bucket == "" || bcfg.StackOutputsKey == "" {
				return fmt.Errorf("no prepared stack found: run `nuon-bundle stack prepare <bundle> --yes` first")
			}
			if !yes {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("--yes is required when stdin is not a TTY")
				}
				fmt.Printf("Create CloudFormation stack %s? [y/N] ", bcfg.StackName)
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					return fmt.Errorf("deploy canceled")
				}
			}

			options := []func(*awsconfig.LoadOptions) error{}
			if bcfg.Profile != "" {
				options = append(options, awsconfig.WithSharedConfigProfile(bcfg.Profile))
			}
			if bcfg.Region != "" {
				options = append(options, awsconfig.WithRegion(bcfg.Region))
			}
			cfg, err := awsconfig.LoadDefaultConfig(cmd.Context(), options...)
			if err != nil {
				return fmt.Errorf("load AWS config: %w", err)
			}
			client := cloudformation.NewFromConfig(cfg)
			fmt.Printf("creating stack %s in %s\n", bcfg.StackName, cfg.Region)
			// The vendor-issued install ID rides along as a tag: it correlates
			// this deployment with the vendor's install record without touching
			// the bundle's frozen physical identity.
			var tags []types.Tag
			if bcfg.InstallID != "" {
				tags = append(tags, types.Tag{Key: stringPtr("nuon:install-id"), Value: &bcfg.InstallID})
			}
			_, err = client.CreateStack(cmd.Context(), &cloudformation.CreateStackInput{
				StackName:   &bcfg.StackName,
				TemplateURL: &bcfg.StackTemplate,
				Capabilities: []types.Capability{
					types.CapabilityCapabilityNamedIam,
					types.CapabilityCapabilityAutoExpand,
				},
				Parameters: []types.Parameter{
					{ParameterKey: stringPtr("PhoneHomeS3Bucket"), ParameterValue: &bcfg.Bucket},
					{ParameterKey: stringPtr("PhoneHomeS3Key"), ParameterValue: &bcfg.StackOutputsKey},
				},
				Tags: tags,
			})
			if err != nil {
				existing, describeErr := client.DescribeStacks(cmd.Context(), &cloudformation.DescribeStacksInput{StackName: &bcfg.StackName})
				if describeErr == nil && len(existing.Stacks) == 1 && existing.Stacks[0].StackStatus == types.StackStatusCreateComplete {
					fmt.Printf("stack %s already exists and is ready\n", bcfg.StackName)
					return nil
				}
				return fmt.Errorf("create stack: %w", err)
			}
			fmt.Println("waiting for stack creation to complete")
			if err := cloudformation.NewStackCreateCompleteWaiter(client).Wait(cmd.Context(), &cloudformation.DescribeStacksInput{StackName: &bcfg.StackName}, 2*time.Hour); err != nil {
				return fmt.Errorf("wait for stack creation: %w", err)
			}
			fmt.Printf("stack %s is ready\n", bcfg.StackName)
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip confirmation")
	return c
}

func stringPtr(value string) *string { return &value }
