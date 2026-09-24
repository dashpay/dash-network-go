package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/dashpay/dash-network-go/internal/files"
	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/plan"
	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/dashpay/dash-network-go/internal/status"
)

const Help = `dashnet — independent Dash network planning, discovery, and EC2 provisioning

Usage:
  dashnet validate  --network network.yaml
  dashnet resolve   --network network.yaml --out release.lock.json
  dashnet plan      --network network.yaml --lock release.lock.json
                    --operation create|upgrade --scope all|platform|core|tenderdash [--out plan.json]
  dashnet inventory --network network.yaml [--profile name] [--out inventory.json]
  dashnet status    --inventory inventory.json [--public] [--max-age 5m] [--out status.json]
  dashnet provision-plan --network network.yaml [--profile name] --out ec2-plan.json
  dashnet provision --plan ec2-plan.json --confirm PLAN_ID [--profile name]
  dashnet operation --plan ec2-plan.json [--profile name]
  dashnet operation-unlock --plan ec2-plan.json --expected-owner RUNNER_ID
                          --confirm-runner-stopped [--profile name]
  dashnet bootstrap-plan --compute-plan ec2-plan.json --lock release.lock.json
                         [--ssh-user ubuntu] [--ssh-port 22] [--address private]
                         [--out bootstrap-plan.json]
  dashnet bootstrap --plan bootstrap-plan.json --confirm BOOTSTRAP_PLAN_ID
                    --ssh-key PATH --known-hosts PATH [--profile name]
                    [--timeout 30m] [--out hosts-ready.json]
  dashnet host-trust --bootstrap-plan bootstrap-plan.json --out known_hosts [--profile name]
  dashnet deployment-plan --bootstrap-plan bootstrap-plan.json --protocol VERSION --out deployment.json
  dashnet deploy --plan deployment.json --confirm PLAN_ID --ssh-key PATH --known-hosts PATH
  dashnet doctor --plan deployment.json --ssh-key PATH --known-hosts PATH [--timeout 3m]
  dashnet stop --plan deployment.json --confirm PLAN_ID --ssh-key PATH --known-hosts PATH
  dashnet upgrade-plan --deployment-plan deployment.json --network candidate.yaml
                       --lock release.lock.json --scope platform|tenderdash --out upgrade.json
  dashnet upgrade --plan upgrade.json --confirm UPGRADE_PLAN_ID --ssh-key PATH --known-hosts PATH
  dashnet version

Planning and discovery are read-only. Provision creates EC2 instances and durable
DynamoDB state: devnet compute only, NOT a working Dash network. operation-unlock
changes a runner claim and requires the previous runner to be stopped first.
Bootstrap installs/verifies Docker and pulls locked images on owned Ubuntu 24.04
hosts; it does not start Core/Platform. Deploy runs the owned devnet lifecycle.
Stop halts owned containers, preserving disks, wallet/validator identities and
AWS resources (billing continues). Doctor is read-only and exits nonzero for
unhealthy/unknown targets. No reset, destroy or testnet mutation yet.
Image availability and EC2 running are not proof of application health.
JSON is written to stdout unless --out is given; files are private (0600),
atomic, and never overwritten. Status is operator data unless --public is used.
`

func Run(ctx context.Context, args []string, out, stderr io.Writer, version string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, Help)
		return err
	}
	if args[0] == "version" {
		_, err := fmt.Fprintln(out, version)
		return err
	}
	switch args[0] {
	case "host-trust":
		return runTrust(ctx, args, out, stderr)
	case "deployment-plan", "deploy", "doctor", "stop", "upgrade-plan", "upgrade":
		return runLifecycle(ctx, args, out, stderr, version)
	case "bootstrap-plan", "bootstrap":
		return runBootstrap(ctx, args, out, stderr, version)
	case "provision-plan", "provision", "operation", "operation-unlock":
		return runProvision(ctx, args, out, stderr, version)
	case "validate", "resolve", "plan", "inventory", "status":
	default:
		return fmt.Errorf("unknown command %q; run dashnet help (see help for supported scoped operations)", args[0])
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	var networkPath, output, lockPath, operation, scope, profile, inventoryPath string
	var public bool
	var maxAge, timeout time.Duration
	flags.DurationVar(&timeout, "timeout", 3*time.Minute, "deadline for this command")
	if args[0] != "status" {
		flags.StringVar(&networkPath, "network", "", "network YAML file (required)")
	}
	if args[0] != "validate" {
		flags.StringVar(&output, "out", "", "new JSON output file; default stdout")
	}
	switch args[0] {
	case "plan":
		flags.StringVar(&lockPath, "lock", "", "release lock file (required)")
		flags.StringVar(&operation, "operation", "", "create or upgrade (required)")
		flags.StringVar(&scope, "scope", "", "all, platform, core, or tenderdash (required)")
	case "inventory":
		flags.StringVar(&profile, "profile", "", "AWS shared profile; omit for OIDC/environment credentials")
	case "status":
		flags.StringVar(&inventoryPath, "inventory", "", "operator inventory JSON (required)")
		flags.BoolVar(&public, "public", false, "export only explicitly public fields; reject private networks")
		flags.DurationVar(&maxAge, "max-age", 5*time.Minute, "mark public observations stale after this age")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments; use named flags")
	}
	if timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if args[0] == "status" {
		if inventoryPath == "" {
			return errors.New("--inventory is required")
		}
		var snapshot inventory.Snapshot
		if err := files.ReadJSON(inventoryPath, &snapshot); err != nil {
			return err
		}
		if err := snapshot.Validate(); err != nil {
			return err
		}
		if !public {
			return emit(out, output, snapshot)
		}
		view, err := status.Project(snapshot, time.Now(), maxAge)
		if err != nil {
			return err
		}
		return emit(out, output, view)
	}
	if networkPath == "" {
		return errors.New("--network is required")
	}
	n, err := spec.Load(networkPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "validate":
		return emit(out, "", struct {
			Valid         bool     `json:"valid"`
			Network       string   `json:"network"`
			SpecHash      string   `json:"specHash"`
			Architectures []string `json:"architectures"`
		}{true, n.Metadata.Name, n.Fingerprint(), n.Architectures()})
	case "resolve":
		lock, err := release.Resolve(ctx, n, release.Registry{}, time.Now())
		if err != nil {
			return err
		}
		return emit(out, output, lock)
	case "plan":
		if lockPath == "" {
			return errors.New("--lock is required")
		}
		var lock release.Lock
		if err := files.ReadJSON(lockPath, &lock); err != nil {
			return err
		}
		preview, err := plan.Build(n, lock, operation, scope)
		if err != nil {
			return err
		}
		return emit(out, output, preview)
	case "inventory":
		opts := []func(*config.LoadOptions) error{config.WithRegion(n.AWS.Region)}
		if profile != "" {
			opts = append(opts, config.WithSharedConfigProfile(profile))
		}
		cfg, err := config.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return fmt.Errorf("load AWS credentials: %w", err)
		}
		snapshot, err := inventory.Collect(ctx, n, sts.NewFromConfig(cfg), ec2.NewFromConfig(cfg), time.Now())
		if err != nil {
			return err
		}
		return emit(out, output, snapshot)
	}
	return errors.New("unreachable command")
}

func emit(out io.Writer, path string, value any) error {
	if path != "" {
		return files.WriteJSON(path, value)
	}
	e := json.NewEncoder(out)
	e.SetIndent("", "  ")
	return e.Encode(value)
}

func Main(ctx context.Context, version string) int {
	if err := Run(ctx, os.Args[1:], os.Stdout, os.Stderr, version); err != nil {
		fmt.Fprintln(os.Stderr, "dashnet:", err)
		return 1
	}
	return 0
}
