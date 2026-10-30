package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/dashpay/dash-network-go/internal/files"
	"github.com/dashpay/dash-network-go/internal/seedtls"
	"github.com/dashpay/dash-network-go/internal/transport"
)

func runSeedTLS(ctx context.Context, args []string, out, stderr io.Writer) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	var targetPath, snapshotPath, planPath, key, hosts, profile, output, confirm, image, email string
	var timeout time.Duration
	fs.StringVar(&targetPath, "target", "", "exact existing DAPI gateway target JSON")
	fs.StringVar(&snapshotPath, "snapshot", "", "read-only seed inspection JSON")
	fs.StringVar(&planPath, "plan", "", "reviewed seed TLS plan JSON")
	fs.StringVar(&key, "ssh-key", "", "SSH identity")
	fs.StringVar(&hosts, "known-hosts", "", "verified instance-scoped known_hosts")
	fs.StringVar(&profile, "profile", "", "AWS profile")
	fs.StringVar(&output, "out", "", "new private JSON result")
	fs.StringVar(&confirm, "confirm", "", "exact plan ID for apply")
	fs.StringVar(&image, "acme-image", "", "digest-pinned lego image")
	fs.StringVar(&email, "acme-email", "", "ACME contact")
	fs.DurationVar(&timeout, "timeout", 30*time.Minute, "operation deadline")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || timeout <= 0 {
		return errors.New("named flags and positive timeout required")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if args[0] == "seed-tls-plan" {
		var s seedtls.Snapshot
		if err := files.ReadJSON(snapshotPath, &s); err != nil {
			return err
		}
		p, err := seedtls.Build(s, image, email, time.Now())
		if err != nil {
			return err
		}
		return emit(out, output, p)
	}
	var t seedtls.Target
	var p seedtls.Plan
	q := seedtls.Request{}
	if args[0] == "seed-tls-apply" || args[0] == "seed-tls-rollback" {
		if err := files.ReadJSON(planPath, &p); err != nil {
			return err
		}
		at := time.Now()
		if args[0] == "seed-tls-rollback" {
			at = p.CreatedAt
		}
		if err := p.Validate(at); err != nil {
			return err
		}
		if confirm != p.ID {
			return errors.New("--confirm must match exact seed TLS plan ID")
		}
		t = p.Snapshot.Target
		q.Action = "apply"
		if args[0] == "seed-tls-rollback" {
			q.Action = "rollback"
		}
		q.Plan = &p
	} else {
		if err := files.ReadJSON(targetPath, &t); err != nil {
			return err
		}
		q.Action = "inspect"
		if args[0] == "seed-tls-doctor" {
			q.Action = "doctor"
		}
	}
	if err := t.Validate(); err != nil {
		return err
	}
	q.Target = t
	// Separate AWS ownership proof from SSH trust; a stale IP cannot inherit authority.
	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(profile), config.WithRegion(t.Region))
	if err != nil {
		return err
	}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return err
	}
	if id.Account == nil || *id.Account != t.Account {
		return errors.New("seed AWS account mismatch")
	}
	d, err := ec2.NewFromConfig(cfg).DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{t.Instance}})
	if err != nil {
		return err
	}
	if len(d.Reservations) != 1 || d.Reservations[0].OwnerId == nil || *d.Reservations[0].OwnerId != t.Account || len(d.Reservations[0].Instances) != 1 {
		return errors.New("seed instance ownership mismatch")
	}
	i := d.Reservations[0].Instances[0]
	if i.InstanceId == nil || *i.InstanceId != t.Instance || i.PublicIpAddress == nil || *i.PublicIpAddress != t.Address || i.State == nil || string(i.State.Name) != "running" {
		return errors.New("seed instance state/address mismatch")
	}
	ssh, err := transport.NewSSH("ubuntu", key, hosts)
	if err != nil {
		return err
	}
	r, err := seedtls.Call(ctx, ssh, q)
	if err != nil {
		return err
	}
	if q.Action == "inspect" {
		return emit(out, output, seedtls.Snapshot{Target: t, ObservedAt: time.Now().UTC(), Baseline: r.Baseline, Fingerprint: r.Fingerprint})
	}
	return emit(out, output, r)
}
