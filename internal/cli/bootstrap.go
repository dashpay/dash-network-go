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
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/dashpay/dash-network-go/internal/transport"
)

func runBootstrap(ctx context.Context, args []string, out, stderr io.Writer, version string) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	var planPath, computePath, lockPath, profile, output, confirm, keyPath, hostsPath string
	var access bootstrap.Access
	var timeout time.Duration
	fs.StringVar(&output, "out", "", "new private JSON file; default stdout")
	if args[0] == "bootstrap-plan" {
		fs.StringVar(&computePath, "compute-plan", "", "existing immutable EC2 provision plan")
		fs.StringVar(&lockPath, "lock", "", "release lock matching the compute network")
		fs.StringVar(&access.User, "ssh-user", "ubuntu", "SSH login with passwordless sudo")
		fs.IntVar(&access.Port, "ssh-port", 22, "SSH port")
		fs.StringVar(&access.Address, "address", "private", "private or public EC2 IPv4")
	} else {
		fs.StringVar(&planPath, "plan", "", "immutable bootstrap plan")
		fs.StringVar(&confirm, "confirm", "", "exact reviewed bootstrap plan ID; authorizes runtime installation and image pulls")
		fs.StringVar(&keyPath, "ssh-key", "", "local private identity file; never copied to nodes or journal")
		fs.StringVar(&hostsPath, "known-hosts", "", "separately verified instance-scoped known_hosts file")
		fs.StringVar(&profile, "profile", "", "AWS profile; omit for OIDC/environment credentials")
		fs.DurationVar(&timeout, "timeout", 30*time.Minute, "command deadline; a disconnect may leave remote work running")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("use named flags, not positional arguments")
	}
	var p bootstrap.Plan
	if args[0] == "bootstrap-plan" {
		if computePath == "" || lockPath == "" {
			return errors.New("--compute-plan and --lock are required")
		}
		var compute provision.Plan
		var lock release.Lock
		if err := files.ReadJSON(computePath, &compute); err != nil {
			return err
		}
		if err := files.ReadJSON(lockPath, &lock); err != nil {
			return err
		}
		var err error
		p, err = bootstrap.Build(compute, lock, access)
		if err != nil {
			return err
		}
		fmt.Fprintln(stderr, "Offline bootstrap plan: Ubuntu 24.04, Docker/Compose, role-specific digest-pinned images; no Dash services start. Host trust and runtime readiness are checked during execution.")
		return emit(out, output, p)
	}
	if planPath == "" || timeout <= 0 {
		return errors.New("--plan and a positive --timeout are required")
	}
	if err := files.ReadJSON(planPath, &p); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if confirm != p.ID {
		return errors.New("--confirm must equal the exact reviewed bootstrap plan ID; no AWS or SSH requests made")
	}
	if keyPath == "" || hostsPath == "" {
		return errors.New("--ssh-key and --known-hosts are required; host trust is never inferred from an unauthenticated scan")
	}
	if output != "" {
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			return errors.New("output already exists or cannot be inspected; choose a new path")
		}
	}
	remote, err := transport.NewSSH(p.Access.User, keyPath, hostsPath)
	if err != nil {
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
	identity := sts.NewFromConfig(cfg)
	if err = provision.VerifyAccount(ctx, p.Compute.Network.AWS.AccountID, identity); err != nil {
		return err
	}
	store := journal.Dynamo{Client: dynamodb.NewFromConfig(cfg), Table: p.Compute.Network.AWS.Provision.StateTable}
	if err = store.Verify(ctx, p.Compute); err != nil {
		return err
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	owner := hex.EncodeToString(random[:])
	fmt.Fprintln(stderr, "runner:", owner)
	r, err := bootstrap.Execute(ctx, p, identity, ec2.NewFromConfig(cfg), store, remote, owner, version, func(s string) { fmt.Fprintln(stderr, s) })
	if err != nil {
		return fmt.Errorf("%w; inspect dashnet operation using the original EC2 plan; resume bootstrap with the same plan, binary and trusted host keys", err)
	}
	return emit(out, output, r)
}
