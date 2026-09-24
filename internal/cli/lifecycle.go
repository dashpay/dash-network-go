package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/files"
	"github.com/dashpay/dash-network-go/internal/journal"
	"github.com/dashpay/dash-network-go/internal/lifecycle"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/transport"
)

func runLifecycle(ctx context.Context, args []string, out, stderr io.Writer, version string) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	var path, bootstrapPath, profile, output, confirm, keyPath, hostsPath string
	var timeout time.Duration
	var protocol uint
	fs.StringVar(&profile, "profile", "", "AWS profile; omit for OIDC/environment credentials")
	fs.StringVar(&output, "out", "", "new private JSON output file")
	fs.DurationVar(&timeout, "timeout", 60*time.Minute, "operation deadline; remote work may survive disconnect")
	if args[0] == "deployment-plan" {
		fs.StringVar(&bootstrapPath, "bootstrap-plan", "", "completed bootstrap plan")
		fs.UintVar(&protocol, "protocol", 0, "explicit initial Platform protocol version, not software major version")
	} else {
		fs.StringVar(&path, "plan", "", "immutable deployment plan")
		fs.StringVar(&keyPath, "ssh-key", "", "private SSH identity file")
		fs.StringVar(&hostsPath, "known-hosts", "", "verified instance-scoped SSH host keys")
		if args[0] != "doctor" {
			fs.StringVar(&confirm, "confirm", "", "exact deployment plan ID authorizing this action")
		}
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || timeout <= 0 {
		return errors.New("use named flags and a positive timeout")
	}
	var b bootstrap.Plan
	var p lifecycle.Plan
	if args[0] == "deployment-plan" {
		if bootstrapPath == "" || protocol < 1 || protocol > 100 {
			return errors.New("--bootstrap-plan and explicit --protocol (1..100) required")
		}
		if err := files.ReadJSON(bootstrapPath, &b); err != nil {
			return err
		}
		if err := b.Validate(); err != nil {
			return err
		}
	} else {
		if path == "" || keyPath == "" || hostsPath == "" {
			return errors.New("--plan, --ssh-key and --known-hosts required")
		}
		if err := files.ReadJSON(path, &p); err != nil {
			return err
		}
		if err := p.Validate(); err != nil {
			return err
		}
		b = p.Bootstrap
		if args[0] != "doctor" && confirm != p.ID {
			return errors.New("--confirm must equal the reviewed deployment plan ID; no AWS or SSH requests made")
		}
	}
	if output != "" {
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			return errors.New("output already exists or cannot be inspected")
		}
	}
	var remote *transport.SSH
	var err error
	if args[0] != "deployment-plan" {
		remote, err = transport.NewSSH(b.Access.User, keyPath, hostsPath)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	opts := []func(*config.LoadOptions) error{config.WithRegion(b.Compute.Network.AWS.Region)}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return err
	}
	identity, cloud := sts.NewFromConfig(cfg), ec2.NewFromConfig(cfg)
	if err = provision.VerifyAccount(ctx, b.Compute.Network.AWS.AccountID, identity); err != nil {
		return err
	}
	store := journal.Dynamo{Client: dynamodb.NewFromConfig(cfg), Table: b.Compute.Network.AWS.Provision.StateTable}
	if err = store.Verify(ctx, b.Compute); err != nil {
		return err
	}
	record, owner, err := store.Read(ctx, b.Compute)
	if err != nil {
		return err
	}
	if args[0] == "deployment-plan" {
		if owner != "" {
			return errors.New("network has an active runner; finish or recover it before planning")
		}
		if record.Bootstrap == nil || record.Bootstrap.PlanID != b.ID || record.Bootstrap.Phase != "hosts-ready" {
			return errors.New("complete bootstrap before binding exact hosts")
		}
		if record.Deployment != nil {
			return errors.New("deployment already exists; retain its original plan, do not regenerate genesis time")
		}
		live, err := provision.RunningTargets(ctx, b.Compute, record, cloud)
		if err != nil {
			return err
		}
		p, err = lifecycle.Build(b, live, uint32(protocol), time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintln(stderr, "Devnet plan: start Core, mine local collateral, register EvoNodes, start Platform and TLS gateway. Existing security groups unchanged. Self-signed TLS; no public DNS/certificates. Review the exact plan and retain this binary.")
		return emit(out, output, p)
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	runner := lifecycle.Runner{Identity: identity, Cloud: cloud, Store: store, Remote: node.Remote{SSH: remote, Access: b.Access, Account: b.Compute.Network.AWS.AccountID, Region: b.Compute.Network.AWS.Region}, Owner: hex.EncodeToString(random[:]), Version: version, Progress: func(s string) { fmt.Fprintln(stderr, s) }}
	if args[0] == "doctor" {
		if owner != "" {
			fmt.Fprintln(stderr, "Network has an active runner; this is a concurrent read-only observation, not permission to mutate.")
		}
		health, err := runner.Doctor(ctx, p, record)
		if err != nil {
			return err
		}
		if err = emit(out, output, health); err != nil {
			return err
		}
		if !health.Healthy {
			return errors.New("network health gate failed; all target failures are in the report")
		}
		return nil
	}
	fmt.Fprintln(stderr, "runner:", runner.Owner)
	result, err := runner.Execute(ctx, p, args[0] == "stop")
	if err != nil {
		return fmt.Errorf("%w; inspect operation using the original EC2 plan; resume deploy with the same deployment plan/binary. Never unlock until the prior runner and remote operations are stopped", err)
	}
	return emit(out, output, result)
}
