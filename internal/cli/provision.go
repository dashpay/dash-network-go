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
	"github.com/dashpay/dash-network-go/internal/files"
	"github.com/dashpay/dash-network-go/internal/journal"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/spec"
)

func runProvision(ctx context.Context, args []string, out, stderr io.Writer, version string) error {
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var networkPath, planPath, profile, output, confirm, expectedOwner string
	var stopped bool
	var timeout time.Duration
	fs.StringVar(&profile, "profile", "", "AWS profile; omit for OIDC/environment credentials")
	fs.StringVar(&output, "out", "", "new private JSON file; default stdout")
	fs.DurationVar(&timeout, "timeout", 15*time.Minute, "command deadline")
	if command == "provision-plan" {
		fs.StringVar(&networkPath, "network", "", "network YAML with aws.provision")
	} else {
		fs.StringVar(&planPath, "plan", "", "immutable EC2 provision plan")
	}
	if command == "provision" {
		fs.StringVar(&confirm, "confirm", "", "exact reviewed plan ID; authorizes billable EC2 creation")
	}
	if command == "operation-unlock" {
		fs.StringVar(&expectedOwner, "expected-owner", "", "exact runner ID from operation inspection")
		fs.BoolVar(&stopped, "confirm-runner-stopped", false, "assert old runner is stopped and its in-flight requests have settled")
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
	var n spec.Network
	var p provision.Plan
	var err error
	if command == "provision-plan" {
		if networkPath == "" {
			return errors.New("--network is required")
		}
		n, err = spec.Load(networkPath)
		if err != nil {
			return err
		}
		if err = n.ValidateProvision(); err != nil {
			return err
		}
	} else {
		if planPath == "" {
			return errors.New("--plan is required")
		}
		if err = files.ReadJSON(planPath, &p); err != nil {
			return err
		}
		if err = p.Validate(); err != nil {
			return err
		}
		n = p.Network
	}
	if command == "provision" && confirm != p.ID {
		return errors.New("--confirm must equal the exact reviewed plan ID; no AWS requests made")
	}
	if command == "operation-unlock" && (!stopped || expectedOwner == "") {
		return errors.New("unlock requires --expected-owner and --confirm-runner-stopped; stop the old runner and allow in-flight requests to settle first")
	}
	// Refuse an already-existing output before any mutation. A later local output
	// failure still cannot roll back AWS; the remote operation remains authoritative.
	if output != "" {
		if _, err = os.Lstat(output); !os.IsNotExist(err) {
			return errors.New("output already exists or cannot be inspected; choose a new path")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	opts := []func(*config.LoadOptions) error{config.WithRegion(n.AWS.Region)}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return err
	}
	identity := sts.NewFromConfig(cfg)
	cloud := ec2.NewFromConfig(cfg)
	if command == "provision-plan" {
		p, err = provision.Prepare(ctx, n, identity, cloud)
		if err != nil {
			return err
		}
	} else if err = provision.VerifyAccount(ctx, n.AWS.AccountID, identity); err != nil {
		return err
	}
	store := journal.Dynamo{Client: dynamodb.NewFromConfig(cfg), Table: n.AWS.Provision.StateTable}
	if err = store.Verify(ctx, p); err != nil {
		return err
	}
	switch command {
	case "provision-plan":
		fmt.Fprintln(stderr, p.Footprint())
		fmt.Fprintln(stderr, "Read-only footprint check, not a cost quote or proof of SSH/OS/chain readiness. Review networking and AMI contents separately.")
		return emit(out, output, p)
	case "operation":
		r, owner, err := store.Read(ctx, p)
		if err != nil {
			return err
		}
		return emit(out, output, struct {
			Owner     string           `json:"owner"`
			Operation provision.Record `json:"operation"`
		}{owner, r})
	case "operation-unlock":
		if _, owner, err := store.Read(ctx, p); err != nil {
			return err
		} else if owner != expectedOwner {
			return errors.New("runner owner differs; nothing changed")
		}
		if err = store.Release(ctx, p, expectedOwner); err != nil {
			return err
		}
		return emit(out, output, struct {
			Released string `json:"releasedOwner"`
			PlanID   string `json:"planId"`
		}{expectedOwner, p.ID})
	case "provision":
		var random [16]byte
		if _, err = rand.Read(random[:]); err != nil {
			return err
		}
		owner := hex.EncodeToString(random[:])
		fmt.Fprintln(stderr, p.Footprint())
		fmt.Fprintln(stderr, "runner:", owner)
		r, err := provision.Execute(ctx, p, identity, cloud, store, owner, version, 5*time.Second, func(s string) { fmt.Fprintln(stderr, s) })
		if err != nil {
			return fmt.Errorf("%w; inspect with dashnet operation --plan %s (same AWS credentials/profile)", err, planPath)
		}
		return emit(out, output, r)
	}
	return errors.New("unreachable provision command")
}
