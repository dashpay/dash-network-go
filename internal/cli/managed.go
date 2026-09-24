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
	"github.com/dashpay/dash-network-go/internal/files"
	"github.com/dashpay/dash-network-go/internal/managed"
	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/dashpay/dash-network-go/internal/transport"
	"io"
	"os"
	"strings"
	"time"
)

func runManaged(ctx context.Context, args []string, out, stderr io.Writer) error {
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var manifestPath, snapshotPath, planPath, key, hosts, profile, output, confirm, scope, operation, choicesPath, expectedOwner string
	var timeout, window time.Duration
	var stopped bool
	fs.StringVar(&profile, "profile", "", "AWS profile; omit for OIDC")
	fs.StringVar(&output, "out", "", "new private JSON output")
	fs.DurationVar(&timeout, "timeout", 60*time.Minute, "bounded operation deadline")
	fs.DurationVar(&window, "observation-window", 90*time.Second, "health observation gap")
	switch command {
	case "managed-import", "managed-operation", "managed-unlock":
		fs.StringVar(&manifestPath, "manifest", "", "explicit existing-network JSON manifest")
	case "managed-enroll", "managed-plan", "managed-doctor":
		fs.StringVar(&snapshotPath, "snapshot", "", "complete existing-state snapshot")
	case "managed-deploy", "managed-upgrade":
		fs.StringVar(&planPath, "plan", "", "reviewed existing-workload operation plan")
	default:
		return errors.New("unknown managed command")
	}
	needsSSH := command != "managed-plan" && command != "managed-operation" && command != "managed-unlock"
	if needsSSH {
		fs.StringVar(&key, "ssh-key", "", "private SSH identity")
		fs.StringVar(&hosts, "known-hosts", "", "independently trusted instance-scoped host keys")
	}
	if command == "managed-enroll" || command == "managed-deploy" || command == "managed-upgrade" {
		fs.StringVar(&confirm, "confirm", "", "exact snapshot/plan ID")
	}
	if command == "managed-plan" {
		fs.StringVar(&scope, "scope", "platform", "core, platform, tenderdash or all")
		fs.StringVar(&operation, "operation", "upgrade", "deploy (same-image restoration) or upgrade")
		fs.StringVar(&choicesPath, "images", "", "JSON component to image candidate map; omitted components retain exact existing images")
	}
	if command == "managed-unlock" {
		fs.StringVar(&expectedOwner, "expected-owner", "", "exact failed runner ID")
		fs.BoolVar(&stopped, "confirm-runner-stopped", false, "old runner and remote operations are stopped")
	}
	if e := fs.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if fs.NArg() != 0 || timeout <= 0 || window <= 0 || window >= timeout {
		return errors.New("named arguments and positive bounded observation/timeout required")
	}
	if needsSSH && (key == "" || hosts == "") {
		return errors.New("--ssh-key and --known-hosts required")
	}
	var f managed.Fleet
	var s managed.Snapshot
	var p managed.Plan
	if manifestPath != "" {
		if e := files.ReadJSON(manifestPath, &f); e != nil {
			return e
		}
	} else if snapshotPath != "" {
		if e := files.ReadJSON(snapshotPath, &s); e != nil {
			return e
		}
		if e := s.Complete(); e != nil {
			return e
		}
		f = s.Fleet
	} else if planPath != "" {
		if e := files.ReadJSON(planPath, &p); e != nil {
			return e
		}
		if e := p.Validate(); e != nil {
			return e
		}
		s = p.Snapshot
		f = s.Fleet
	} else {
		return errors.New("explicit --manifest, --snapshot or --plan required")
	}
	if e := f.Validate(); e != nil {
		return e
	}
	if command == "managed-enroll" && confirm != s.ID {
		return errors.New("--confirm must equal reviewed snapshot ID; no AWS/SSH calls made")
	}
	if command == "managed-deploy" || command == "managed-upgrade" {
		if confirm != p.ID || strings.TrimPrefix(command, "managed-") != p.Operation {
			return errors.New("operation/confirmation does not match reviewed plan; no AWS/SSH calls made")
		}
	}
	if command == "managed-unlock" && (!stopped || expectedOwner == "") {
		return errors.New("exact owner and stopped-runner assertion required")
	}
	if output != "" {
		if _, e := os.Lstat(output); !os.IsNotExist(e) {
			return errors.New("output already exists or cannot be inspected")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	opts := []func(*config.LoadOptions) error{config.WithRegion(f.Region)}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, e := config.LoadDefaultConfig(ctx, opts...)
	if e != nil {
		return e
	}
	identity, cloud := sts.NewFromConfig(cfg), ec2.NewFromConfig(cfg)
	if e = managed.VerifyCloud(ctx, f, identity, cloud); e != nil {
		return e
	}
	var remote managed.Backend
	if needsSSH {
		ssh, e := transport.NewSSH(f.Access.User, key, hosts)
		if e != nil {
			return e
		}
		remote = managed.Remote{SSH: ssh}
	}
	if command == "managed-import" {
		s = managed.Observe(ctx, f, remote, 0)
		if e = emit(out, output, s); e != nil {
			return e
		}
		return s.Complete()
	}
	if command == "managed-doctor" {
		h, e := managed.Doctor(ctx, s, remote, window, func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		})
		if e != nil {
			return e
		}
		if e = emit(out, output, h); e != nil {
			return e
		}
		if !h.Healthy {
			return errors.New("managed health gate failed; all targets retained in report")
		}
		return nil
	}
	store := managed.Dynamo{Client: dynamodb.NewFromConfig(cfg), Table: f.StateTable}
	if e = store.Verify(ctx, f); e != nil {
		return e
	}
	if command == "managed-operation" {
		r, owner, e := store.Read(ctx, f)
		if e != nil {
			return e
		}
		return emit(out, output, struct {
			Record managed.Record `json:"record"`
			Owner  string         `json:"owner"`
		}{r, owner})
	}
	if command == "managed-unlock" {
		return store.Release(ctx, f, expectedOwner)
	}
	if command == "managed-plan" {
		record, owner, e := store.Read(ctx, f)
		if e != nil {
			return e
		}
		if owner != "" || (record.Phase != "enrolled" && record.Phase != "complete") {
			return errors.New("finish/recover current operation before planning")
		}
		choices := map[string]string{}
		if choicesPath != "" {
			if e = files.ReadJSON(choicesPath, &choices); e != nil {
				return e
			}
		}
		if operation == "deploy" && len(choices) > 0 {
			return errors.New("deploy restores existing images; use upgrade for version changes")
		}
		images, e := managed.Resolve(ctx, s, scope, choices, release.Registry{})
		if e != nil {
			return e
		}
		p, e = managed.Build(s, operation, scope, record.OperationID, images, time.Now())
		if e != nil {
			return e
		}
		return emit(out, output, p)
	}
	var ownerBytes [16]byte
	if _, e = rand.Read(ownerBytes[:]); e != nil {
		return e
	}
	owner := hex.EncodeToString(ownerBytes[:])
	fmt.Fprintln(stderr, "runner:", owner)
	runner := managed.Runner{Identity: identity, Cloud: cloud, Store: store, Remote: remote, Owner: owner, Window: window, Progress: func(s string) { fmt.Fprintln(stderr, s) }}
	var record managed.Record
	if command == "managed-enroll" {
		record, e = runner.Enroll(ctx, s)
	} else {
		record, e = runner.Execute(ctx, p)
	}
	if e != nil {
		return fmt.Errorf("%w; inspect managed-operation and resume the same artifact; no reset or automatic rollback", e)
	}
	return emit(out, output, record)
}
