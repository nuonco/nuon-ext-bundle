package cmd

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

type planItem struct {
	Member            bundle.Member
	Repo, Tag, Status string
}
type ecrAPI interface {
	GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
	CreateRepository(context.Context, *ecr.CreateRepositoryInput, ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error)
}

func pushCmd() *cobra.Command {
	var registry, prefix, profile, region string
	var dryRun, yes bool
	var concurrency int
	c := &cobra.Command{Use: "push <bundle.tar.zst>", Short: "push bundle artifacts into customer ECR/S3", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		bcfg, err := loadBundleConfig()
		if err != nil {
			return err
		}
		registry = fallback(registry, bcfg.ECRRegistry)
		prefix = fallback(prefix, bcfg.ECRPrefix)
		profile = fallback(profile, bcfg.Profile)
		region = fallback(region, bcfg.Region)
		o, err := openArchive(cmd, args[0])
		if err != nil {
			return err
		}
		defer o.cleanup()
		if err := bundle.VerifyBlobs(o.dir); err != nil {
			return err
		}
		if registry == "" {
			return fmt.Errorf("--ecr is required (or set by the active context)")
		}
		items := makePlan(o.bundle.Members(), prefix)
		fmt.Printf("checksum sha256:%s  %d artifacts\n", o.checksum, len(items))
		printPlan(items)
		if dryRun {
			return nil
		}
		if !yes {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("--yes is required when stdin is not a TTY")
			}
			fmt.Print("Push these artifacts? [y/N] ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) != "y" {
				return fmt.Errorf("push canceled")
			}
		}
		options := []func(*config.LoadOptions) error{}
		if profile != "" {
			options = append(options, config.WithSharedConfigProfile(profile))
		}
		if region == "" {
			region = regionFromRegistry(registry)
		}
		if region != "" {
			options = append(options, config.WithRegion(region))
		}
		cfg, err := config.LoadDefaultConfig(cmd.Context(), options...)
		if err != nil {
			return fmt.Errorf("unable to load AWS config: %w", err)
		}
		identity, err := sts.NewFromConfig(cfg).GetCallerIdentity(cmd.Context(), &sts.GetCallerIdentityInput{})
		if err != nil {
			return fmt.Errorf("unable to get AWS identity: %w", err)
		}
		fmt.Printf("AWS identity %s  region %s\n", aws.ToString(identity.Arn), cfg.Region)
		ecrClient := ecr.NewFromConfig(cfg)
		credential, err := ecrCredential(cmd.Context(), ecrClient)
		if err != nil {
			return err
		}
		start := time.Now()
		pushed, skipped, failed := 0, 0, 0
		for i := range items {
			itemStart := time.Now()
			desc := rootFor(o.bundle, items[i].Member.Digest.String())
			if err := ensureRepo(cmd.Context(), ecrClient, items[i].Repo); err != nil {
				items[i].Status = "failed"
				failed++
				fmt.Printf("%s failed: %v\n", items[i].Member.Key, err)
				continue
			}
			repo, err := remote.NewRepository(registry + "/" + items[i].Repo)
			if err != nil {
				items[i].Status = "failed"
				failed++
				continue
			}
			repo.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache(), Credential: auth.StaticCredential(registry, credential)}
			exists, err := repo.Exists(cmd.Context(), desc)
			if err == nil && exists {
				if err = repo.Tag(cmd.Context(), desc, items[i].Tag); err == nil {
					items[i].Status = "skipped"
					skipped++
				}
			} else if err == nil {
				if _, err = oras.Copy(cmd.Context(), o.bundle.Store(), desc.Digest.String(), repo, items[i].Tag, oras.CopyOptions{CopyGraphOptions: oras.CopyGraphOptions{Concurrency: concurrency}}); err == nil {
					items[i].Status = "pushed"
					pushed++
				}
			}
			if err != nil {
				items[i].Status = "failed"
				failed++
				fmt.Printf("%s failed: %v\n", items[i].Member.Key, err)
				continue
			}
			fmt.Printf("%s %s (%s, %s)\n", items[i].Member.Key, items[i].Status, humanSize(items[i].Member.Size), time.Since(itemStart).Round(time.Millisecond))
		}
		fmt.Printf("%d pushed, %d skipped, %d failed in %s\n", pushed, skipped, failed, time.Since(start).Round(time.Millisecond))
		receipt := struct {
			Registry    string     `json:"registry"`
			Prefix      string     `json:"prefix"`
			AWSIdentity string     `json:"aws_identity"`
			PushedAt    time.Time  `json:"pushed_at"`
			Items       []planItem `json:"items"`
		}{registry, prefix, aws.ToString(identity.Arn), time.Now().UTC(), items}
		data, _ := json.MarshalIndent(receipt, "", "  ")
		if err := os.WriteFile(receiptPath(args[0]), data, 0644); err != nil {
			return fmt.Errorf("unable to write receipt: %w", err)
		}
		if failed > 0 {
			return fmt.Errorf("%d artifacts failed", failed)
		}
		bcfg.ECRRegistry, bcfg.ECRPrefix, bcfg.Profile, bcfg.Region = registry, prefix, profile, region
		if ref := runnerImageRef(registry, items); ref != "" {
			bcfg.Image = ref
		}
		if err := saveBundleConfig(bcfg); err != nil {
			fmt.Fprintf(os.Stderr, "warning: unable to save context: %v\n", err)
		} else {
			fmt.Println("saved ECR settings to the active context")
		}
		return nil
	}}
	c.Flags().StringVar(&registry, "ecr", "", "ECR registry hostname")
	c.Flags().StringVar(&prefix, "prefix", "", "repository prefix")
	c.Flags().StringVar(&profile, "profile", "", "AWS profile")
	c.Flags().StringVar(&region, "region", "", "AWS region")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without pushing")
	c.Flags().BoolVar(&yes, "yes", false, "skip confirmation")
	c.Flags().IntVar(&concurrency, "concurrency", 3, "copy concurrency")
	return c
}

var invalidRepo = regexp.MustCompile(`[^a-z0-9._/-]+`)

func sanitize(v string) string {
	parts := strings.Split(invalidRepo.ReplaceAllString(strings.ToLower(v), "-"), "/")
	for i := range parts {
		parts[i] = strings.Trim(parts[i], ".-_")
	}
	return strings.Join(parts, "/")
}
func makePlan(ms []bundle.Member, prefix string) []planItem {
	out := make([]planItem, 0, len(ms))
	for _, m := range ms {
		name := m.Name
		switch m.Kind {
		case "sandbox":
			name = "sandbox"
		case "action-artifact":
			name = "actions/" + m.Name
		case "stack-asset":
			name = "stack-assets/" + m.Name
		}
		repo := sanitize(strings.Trim(prefix+"/"+name, "/"))
		out = append(out, planItem{Member: m, Repo: repo, Tag: "sha256-" + m.Digest.Encoded()})
	}
	return out
}

func runnerImageRef(registry string, items []planItem) string {
	for _, i := range items {
		if i.Member.Key == "runner:image" && (i.Status == "pushed" || i.Status == "skipped") {
			return registry + "/" + i.Repo + ":" + i.Tag
		}
	}
	return ""
}

func printPlan(items []planItem) {
	for _, i := range items {
		fmt.Printf("%s  %s:%s  %s\n", i.Member.Key, i.Repo, i.Tag, humanSize(i.Member.Size))
	}
}
func regionFromRegistry(v string) string {
	p := strings.Split(v, ".")
	if len(p) >= 6 && p[1] == "dkr" && p[2] == "ecr" {
		return p[3]
	}
	return ""
}
func rootFor(b *bundle.Bundle, dgst string) (d ocispec.Descriptor) {
	for _, d = range b.Roots {
		if d.Digest.String() == dgst {
			return d
		}
	}
	return d
}
func ensureRepo(ctx context.Context, client ecrAPI, name string) error {
	_, err := client.CreateRepository(ctx, &ecr.CreateRepositoryInput{RepositoryName: aws.String(name)})
	var exists *ecrtypes.RepositoryAlreadyExistsException
	if errors.As(err, &exists) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("unable to create repository %s: %w", name, err)
	}
	return nil
}
func ecrCredential(ctx context.Context, client ecrAPI) (auth.Credential, error) {
	out, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return auth.Credential{}, fmt.Errorf("unable to get ECR authorization token: %w", err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return auth.Credential{}, fmt.Errorf("unable to get ECR authorization token")
	}
	raw, err := base64.StdEncoding.DecodeString(*out.AuthorizationData[0].AuthorizationToken)
	if err != nil {
		return auth.Credential{}, fmt.Errorf("unable to decode ECR authorization token: %w", err)
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return auth.Credential{}, fmt.Errorf("unable to parse ECR authorization token")
	}
	return auth.Credential{Username: user, Password: pass}, nil
}
