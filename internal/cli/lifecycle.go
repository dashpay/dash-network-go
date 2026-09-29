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
	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/dashpay/dash-network-go/internal/transport"
)

func runLifecycle(ctx context.Context, args []string, out, stderr io.Writer, version string) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	var path, bootstrapPath, profile, output, confirm, keyPath, hostsPath string
	var candidatePath, lockPath, scope, advertise, gatewayTLS, acmeEmail string
	var timeout, observationWindow time.Duration
	var protocol, blockSeconds, epochSeconds uint
	var coreOnly bool
	fs.StringVar(&profile, "profile", "", "AWS profile; omit for OIDC/environment credentials")
	fs.StringVar(&output, "out", "", "new private JSON output file")
	fs.DurationVar(&timeout, "timeout", 60*time.Minute, "operation deadline; remote work may survive disconnect")
	fs.DurationVar(&observationWindow, "observation-window", 15*time.Second, "minimum interval between health samples; allow for consensus round timeouts")
	if args[0] == "deployment-plan" {
		fs.StringVar(&bootstrapPath, "bootstrap-plan", "", "completed bootstrap plan")
		fs.UintVar(&protocol, "protocol", 0, "explicit initial Platform protocol version, not software major version")
		fs.StringVar(&advertise, "advertise", "auto", "service addresses to register: public or auto (IPAM Elastic IPs; dashmate's Core never uses private addresses)")
		fs.StringVar(&gatewayTLS, "gateway-tls", "auto", "gateway certificates: letsencrypt, letsencrypt-staging, self-signed, or auto (letsencrypt with public addresses, an acme image and --acme-email)")
		fs.StringVar(&acmeEmail, "acme-email", "", "ACME account contact for trusted gateway certificates")
		fs.UintVar(&blockSeconds, "block-time", lifecycle.DefaultBlockSeconds, fmt.Sprintf("Core block interval in seconds (%d..%d): powtargetspacing and the miner's cadence", lifecycle.MinBlockSeconds, lifecycle.MaxBlockSeconds))
		fs.UintVar(&epochSeconds, "epoch-time", lifecycle.DefaultEpochSeconds, fmt.Sprintf("Platform epoch length in seconds (%d..%d): Drive's EPOCH_TIME_LENGTH_S", lifecycle.MinEpochSeconds, lifecycle.MaxEpochSeconds))
	} else if args[0] == "upgrade-plan" {
		fs.StringVar(&path, "deployment-plan", "", "original immutable deployment plan")
		fs.StringVar(&candidatePath, "network", "", "candidate network definition; images only may change")
		fs.StringVar(&lockPath, "lock", "", "resolved candidate release lock")
		fs.StringVar(&scope, "scope", "platform", "platform or tenderdash (Core preserved), or core (Core only, every node)")
	} else {
		fs.StringVar(&path, "plan", "", "immutable deployment or upgrade plan")
		fs.StringVar(&keyPath, "ssh-key", "", "private SSH identity file")
		fs.StringVar(&hostsPath, "known-hosts", "", "verified instance-scoped SSH host keys")
		if args[0] != "doctor" {
			fs.StringVar(&confirm, "confirm", "", "exact operation plan ID authorizing this action")
		}
		if args[0] == "deploy" {
			fs.BoolVar(&coreOnly, "core-only", false, "stop once Core is mining with DKG enabled; quorums then form on their own, and deploy again (same plan) starts Platform after them")
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
	if observationWindow <= 0 || ((args[0] == "doctor" || args[0] == "deploy" || args[0] == "upgrade") && observationWindow >= timeout) {
		return errors.New("--observation-window must be positive and leave time for probes inside --timeout")
	}
	var b bootstrap.Plan
	var p lifecycle.Plan
	var upgrade lifecycle.UpgradePlan
	var candidate spec.Network
	var candidateLock release.Lock
	if args[0] == "deployment-plan" {
		if bootstrapPath == "" || protocol < 1 || protocol > 100 {
			return errors.New("--bootstrap-plan and explicit --protocol (1..100) required")
		}
		if advertise == "private" {
			return errors.New("--advertise private is not supported: dashmate's Core never connects to private addresses (allowprivatenet=0); allocate IPAM Elastic IPs")
		}
		if advertise != "auto" && advertise != "public" {
			return errors.New("--advertise must be auto or public")
		}
		if gatewayTLS != "auto" && gatewayTLS != "self-signed" && node.ACMEIssuers[gatewayTLS] == "" {
			return errors.New("--gateway-tls must be auto, self-signed, letsencrypt or letsencrypt-staging")
		}
		if blockSeconds < lifecycle.MinBlockSeconds || blockSeconds > lifecycle.MaxBlockSeconds {
			return fmt.Errorf("--block-time must be %d..%d seconds", lifecycle.MinBlockSeconds, lifecycle.MaxBlockSeconds)
		}
		if epochSeconds < lifecycle.MinEpochSeconds || epochSeconds > lifecycle.MaxEpochSeconds {
			return fmt.Errorf("--epoch-time must be %d..%d seconds", lifecycle.MinEpochSeconds, lifecycle.MaxEpochSeconds)
		}
		if err := files.ReadJSON(bootstrapPath, &b); err != nil {
			return err
		}
		if err := b.Validate(); err != nil {
			return err
		}
	} else if args[0] == "upgrade-plan" {
		if path == "" || candidatePath == "" || lockPath == "" {
			return errors.New("--deployment-plan, --network and --lock required")
		}
		if err := files.ReadJSON(path, &p); err != nil {
			return err
		}
		if err := p.Validate(); err != nil {
			return err
		}
		var err error
		candidate, err = spec.Load(candidatePath)
		if err != nil {
			return err
		}
		if err = files.ReadJSON(lockPath, &candidateLock); err != nil {
			return err
		}
		if err = candidateLock.Validate(candidate); err != nil {
			return err
		}
		b = p.Bootstrap
	} else {
		if path == "" || keyPath == "" || hostsPath == "" {
			return errors.New("--plan, --ssh-key and --known-hosts required")
		}
		if args[0] == "upgrade" {
			if err := files.ReadJSON(path, &upgrade); err != nil {
				return err
			}
			if err := upgrade.Validate(); err != nil {
				return err
			}
			p = upgrade.Deployment
		} else {
			if err := files.ReadJSON(path, &p); err != nil {
				return err
			}
		}
		if err := p.Validate(); err != nil {
			return err
		}
		// Doctor stretches the window to 2.5 blocks on slower chains.
		if effective := max(observationWindow, time.Duration(p.MiningIntervalSeconds)*5*time.Second/2); p.MiningIntervalSeconds > lifecycle.DefaultBlockSeconds && effective >= timeout && args[0] != "stop" {
			return fmt.Errorf("--timeout must exceed the %s observation window this %ds-block chain needs", effective, p.MiningIntervalSeconds)
		}
		b = p.Bootstrap
		expectedID := p.ID
		if args[0] == "upgrade" {
			expectedID = upgrade.ID
		}
		if args[0] != "doctor" && confirm != expectedID {
			return errors.New("--confirm must equal the reviewed operation plan ID; no AWS or SSH requests made")
		}
	}
	if output != "" {
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			return errors.New("output already exists or cannot be inspected")
		}
	}
	var remote *transport.SSH
	var err error
	if args[0] != "deployment-plan" && args[0] != "upgrade-plan" {
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
		public := ipamAddresses(b, record)
		if public == nil {
			return errors.New("dashmate-rendered devnets advertise public addresses: every host needs an IPAM Elastic IP (see docs/ipam.md)")
		}
		opts := lifecycle.Options{Public: public, BlockSeconds: int(blockSeconds), EpochSeconds: int(epochSeconds)}
		if gatewayTLS == "auto" && public != nil && acmeEmail != "" && hasImage(b, "acme") {
			gatewayTLS = "letsencrypt"
		}
		if gatewayTLS != "auto" && gatewayTLS != "self-signed" {
			opts.ACMEIssuer, opts.ACMEEmail = gatewayTLS, acmeEmail
		}
		p, err = lifecycle.BuildWith(b, live, opts, uint32(protocol), time.Now())
		if err != nil {
			return err
		}
		mode := "public IPAM service addresses (security groups must allow Core 20001 and Tenderdash 26656 from the fleet's public IPs)"
		certs := "persisted self-signed gateway certificates"
		if p.GatewayTLS != nil {
			certs = p.GatewayTLS.Issuer + " certificates for each validator's public IP (ACME HTTP-01: port 80 must be reachable)"
		}
		fmt.Fprintln(stderr, "Devnet plan: each node's services are rendered by its release's dashmate and run from dashmate's compose files. Start Core, mine local collateral, register EvoNodes with "+mode+", start Platform and TLS gateway with "+certs+". Existing security groups unchanged. Review the exact plan and retain this binary.")
		return emit(out, output, p)
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	runner := lifecycle.Runner{Identity: identity, Cloud: cloud, Store: store, Remote: node.Remote{SSH: remote, Access: b.Access, Account: b.Compute.Network.AWS.AccountID, Region: b.Compute.Network.AWS.Region}, Owner: hex.EncodeToString(random[:]), Version: version, Progress: func(s string) { fmt.Fprintln(stderr, s) }}
	runner.ObservationWindow = observationWindow
	runner.CoreOnly = coreOnly
	// Anonymous: pins the sidecar images each release's dashmate requests.
	runner.Registry = release.Registry{}
	if args[0] == "upgrade-plan" {
		if owner != "" {
			return errors.New("network has an active runner; finish or recover it before upgrade planning")
		}
		upgrade, err = runner.PrepareUpgrade(ctx, p, record, candidate, candidateLock, scope)
		if err != nil {
			return err
		}
		fmt.Fprintln(stderr, "Forward-only image rollout on the exact owned devnet. Source images come from shared state; live digests, health, membership and Core preservation are checked before execution. No protocol migration, reset or automatic downgrade.")
		return emit(out, output, upgrade)
	}
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
	if args[0] == "upgrade" {
		result, err := runner.Upgrade(ctx, upgrade)
		if err != nil {
			return fmt.Errorf("%w; inspect shared operation and resume upgrade with this same plan/binary; do not reset or blindly downgrade", err)
		}
		return emit(out, output, result)
	}
	result, err := runner.Execute(ctx, p, args[0] == "stop")
	if err != nil {
		return fmt.Errorf("%w; inspect operation using the original EC2 plan; resume deploy with the same deployment plan/binary. Never unlock until the prior runner and remote operations are stopped", err)
	}
	return emit(out, output, result)
}

// ipamAddresses maps every target to the Elastic IP this network allocated
// from IPAM, or returns nil when any host has none.
func ipamAddresses(b bootstrap.Plan, r provision.Record) map[string]string {
	if !b.Compute.Network.AWS.Provision.PublicIPv4 || b.Compute.Network.AWS.Provision.IPAMPoolID == "" {
		return nil
	}
	out := map[string]string{}
	for _, t := range b.Compute.Targets {
		n, ok := r.Nodes[t.Name]
		if !ok || n.Address == nil || n.Address.PublicIP == "" {
			return nil
		}
		out[t.Name] = n.Address.PublicIP
	}
	return out
}

func hasImage(b bootstrap.Plan, component string) bool {
	for _, image := range b.Release.Images {
		if image.Component == component {
			return true
		}
	}
	return false
}
