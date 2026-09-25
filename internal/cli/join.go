package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/files"
	"github.com/dashpay/dash-network-go/internal/join"
	"github.com/dashpay/dash-network-go/internal/journal"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/transport"
	"io"
	"os"
	"time"
)

func runJoin(ctx context.Context, args []string, out, stderr io.Writer) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	var bootstrapPath, chainPath, path, profile, output, confirm, key, hosts string
	var timeout time.Duration
	fs.StringVar(&profile, "profile", "", "AWS profile; omit for OIDC")
	fs.StringVar(&output, "out", "", "new private JSON file")
	fs.DurationVar(&timeout, "timeout", 4*time.Hour, "join sync deadline; interruption preserves node data")
	if args[0] == "join-plan" {
		fs.StringVar(&bootstrapPath, "bootstrap-plan", "", "completed fullnode bootstrap plan")
		fs.StringVar(&chainPath, "chain", "", "reviewed public chain contract: genesis, checkpoint, peers and options")
	} else {
		fs.StringVar(&path, "plan", "", "retained Core join plan")
		fs.StringVar(&confirm, "confirm", "", "exact reviewed join plan ID")
		fs.StringVar(&key, "ssh-key", "", "private SSH key file")
		fs.StringVar(&hosts, "known-hosts", "", "authenticated instance-scoped host keys")
	}
	if e := fs.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if fs.NArg() != 0 || timeout <= 0 {
		return errors.New("named flags and positive timeout required")
	}
	var b bootstrap.Plan
	var p join.Plan
	var chain node.CoreJoin
	if args[0] == "join-plan" {
		if bootstrapPath == "" || chainPath == "" {
			return errors.New("--bootstrap-plan and --chain required")
		}
		if e := files.ReadJSON(bootstrapPath, &b); e != nil {
			return e
		}
		if e := files.ReadJSON(chainPath, &chain); e != nil {
			return e
		}
		if e := chain.Validate(); e != nil {
			return e
		}
	} else {
		if path == "" || key == "" || hosts == "" {
			return errors.New("--plan, --ssh-key and --known-hosts required")
		}
		if e := files.ReadJSON(path, &p); e != nil {
			return e
		}
		if e := p.Validate(); e != nil {
			return e
		}
		if confirm != p.ID {
			return errors.New("--confirm must equal exact join plan ID; no network calls made")
		}
		b = p.Bootstrap
	}
	if e := b.Validate(); e != nil {
		return e
	}
	if output != "" {
		if _, e := os.Lstat(output); !os.IsNotExist(e) {
			return errors.New("output exists or cannot be inspected")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	opts := []func(*config.LoadOptions) error{config.WithRegion(b.Compute.Network.AWS.Region)}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, e := config.LoadDefaultConfig(ctx, opts...)
	if e != nil {
		return e
	}
	identity := sts.NewFromConfig(cfg)
	cloud := ec2.NewFromConfig(cfg)
	if e = provision.VerifyAccount(ctx, b.Compute.Network.AWS.AccountID, identity); e != nil {
		return e
	}
	store := journal.Dynamo{Client: dynamodb.NewFromConfig(cfg), Table: b.Compute.Network.AWS.Provision.StateTable}
	if e = store.Verify(ctx, b.Compute); e != nil {
		return e
	}
	record, owner, e := store.Read(ctx, b.Compute)
	if e != nil {
		return e
	}
	if args[0] == "join-plan" {
		if owner != "" || record.Deployment != nil || record.Join != nil || record.Bootstrap == nil || record.Bootstrap.PlanID != b.ID || record.Bootstrap.Phase != "hosts-ready" {
			return errors.New("allocation must have completed fresh bootstrap and no deployment/runner; retain existing join plan for resume")
		}
		live, e := provision.RunningTargets(ctx, b.Compute, record, cloud)
		if e != nil {
			return e
		}
		p, e = join.Build(b, chain, live)
		if e != nil {
			return e
		}
		fmt.Fprintln(stderr, "New Core fullnodes only: join reviewed existing chain, disable wallets/mining, preserve source fleet. No validator registration or new genesis.")
		return emit(out, output, p)
	}
	remote, e := transport.NewSSH(b.Access.User, key, hosts)
	if e != nil {
		return e
	}
	var random [16]byte
	if _, e = rand.Read(random[:]); e != nil {
		return e
	}
	runner := join.Runner{Identity: identity, Cloud: cloud, Store: store, Remote: node.Remote{SSH: remote, Access: b.Access, Account: b.Compute.Network.AWS.AccountID, Region: b.Compute.Network.AWS.Region}, Owner: hex.EncodeToString(random[:]), Progress: func(s string) { fmt.Fprintln(stderr, s) }}
	fmt.Fprintln(stderr, "runner:", runner.Owner)
	result, e := runner.Execute(ctx, p)
	if e != nil {
		return fmt.Errorf("%w; inspect operation using the allocation EC2 plan; resume join with this same plan and binary", e)
	}
	return emit(out, output, result)
}
