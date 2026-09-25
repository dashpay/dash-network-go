package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/files"
	"github.com/dashpay/dash-network-go/internal/journal"
)

func runTrust(ctx context.Context, args []string, out, stderr io.Writer) error {
	fs := flag.NewFlagSet("host-trust", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var planPath, profile, output string
	var timeout time.Duration
	fs.StringVar(&planPath, "bootstrap-plan", "", "immutable bootstrap plan")
	fs.StringVar(&profile, "profile", "", "AWS profile; omit for OIDC/environment credentials")
	fs.StringVar(&output, "out", "", "required new private instance-scoped known_hosts file")
	fs.DurationVar(&timeout, "timeout", 3*time.Minute, "read-only collection deadline")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || planPath == "" || output == "" || timeout <= 0 {
		return errors.New("host-trust requires --bootstrap-plan, a new --out path and a positive timeout")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return errors.New("output already exists or cannot be inspected; existing host trust is never replaced")
	}
	var p bootstrap.Plan
	if err := files.ReadJSON(planPath, &p); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	opts := []func(*config.LoadOptions) error{config.WithRegion(p.Compute.Network.AWS.Region)}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return err
	}
	store := journal.Dynamo{Client: dynamodb.NewFromConfig(cfg), Table: p.Compute.Network.AWS.Provision.StateTable}
	if err = store.Verify(ctx, p.Compute); err != nil {
		return err
	}
	cloud := ec2.NewFromConfig(cfg)
	hosts, err := bootstrap.CollectTrust(ctx, p, sts.NewFromConfig(cfg), cloud, cloud, store, time.Now().UTC())
	if err != nil {
		return err
	}
	var text strings.Builder
	for _, h := range hosts {
		text.WriteString(h.KnownHostsLine + "\n")
	}
	if err = files.WriteText(output, text.String()); err != nil {
		return err
	}
	for _, h := range hosts {
		fmt.Fprintf(out, "%s %s %s\n", h.Node, h.InstanceID, h.Fingerprint)
	}
	fmt.Fprintln(stderr, "Published all instance-scoped host keys from authenticated EC2 console output; no cloud or node changes.")
	return nil
}
