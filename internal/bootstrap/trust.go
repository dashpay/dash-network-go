package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/provision"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type ConsoleAPI interface {
	GetConsoleOutput(context.Context, *ec2.GetConsoleOutputInput, ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error)
}

type HostTrust struct {
	Node, InstanceID, Fingerprint, KnownHostsLine string
}

// ConsoleHostKey uses cloud-init's public-key block from the authenticated EC2
// control plane, never an unauthenticated SSH scan. Raw console logs may contain
// private data and must never appear in output, journal entries or error text.
func ConsoleHostKey(encoded string) (ssh.PublicKey, error) {
	if len(encoded) == 0 || len(encoded) > 1<<20 {
		return nil, errors.New("missing or oversized EC2 console output")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("invalid EC2 console encoding")
	}
	const begin = "-----BEGIN SSH HOST KEY KEYS-----"
	const end = "-----END SSH HOST KEY KEYS-----"
	active, complete := false, false
	var key ssh.PublicKey
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch line {
		case begin:
			if active || complete {
				return nil, errors.New("ambiguous cloud-init host-key blocks; verify trust independently")
			}
			active = true
		case end:
			if !active {
				return nil, errors.New("incomplete cloud-init host-key block")
			}
			active, complete = false, true
		default:
			if !active || !strings.HasPrefix(line, ssh.KeyAlgoED25519+" ") {
				continue
			}
			parsed, _, options, rest, parseErr := ssh.ParseAuthorizedKey([]byte(line))
			if parseErr != nil || len(options) != 0 || len(rest) != 0 || parsed.Type() != ssh.KeyAlgoED25519 || key != nil {
				return nil, errors.New("invalid or ambiguous cloud-init Ed25519 host key")
			}
			key = parsed
		}
	}
	if active || !complete || key == nil {
		return nil, errors.New("complete cloud-init Ed25519 host-key block not available; retry or verify trust independently")
	}
	return key, nil
}

// CollectTrust requires all journaled instances to be running and immutable.
// Publication is all-or-nothing, after a second fleet/claim check. It never
// rewrites existing trust, SSH keys, instances, or a journal.
func CollectTrust(ctx context.Context, p Plan, identity inventory.STS, cloud provision.EC2, console ConsoleAPI, store Store, now time.Time) ([]HostTrust, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := provision.VerifyAccount(ctx, p.Compute.Network.AWS.AccountID, identity); err != nil {
		return nil, err
	}
	r, owner, err := store.Read(ctx, p.Compute)
	if err != nil {
		return nil, err
	}
	if owner != "" {
		return nil, errors.New("host trust requires an idle network operation")
	}
	live, err := provision.RunningTargets(ctx, p.Compute, r, cloud)
	if err != nil {
		return nil, err
	}
	hosts, fingerprints := []HostTrust{}, map[string]bool{}
	for _, target := range p.Compute.Targets {
		instance := live[target.Name]
		id := aws.ToString(instance.InstanceId)
		output, err := console.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{InstanceId: aws.String(id), Latest: aws.Bool(true)})
		if err != nil {
			return nil, fmt.Errorf("read authenticated EC2 console for %s: %w", target.Name, err)
		}
		if output == nil || aws.ToString(output.InstanceId) != id || output.Timestamp == nil || instance.LaunchTime == nil || output.Timestamp.Before(*instance.LaunchTime) || output.Timestamp.After(now.Add(5*time.Minute)) {
			return nil, fmt.Errorf("missing, stale or mismatched EC2 console identity for %s", target.Name)
		}
		key, err := ConsoleHostKey(aws.ToString(output.Output))
		if err != nil {
			return nil, fmt.Errorf("host trust for %s: %w", target.Name, err)
		}
		fingerprint := ssh.FingerprintSHA256(key)
		if fingerprints[fingerprint] {
			return nil, errors.New("duplicate SSH host identity across instances; refuse cloned host keys")
		}
		fingerprints[fingerprint] = true
		alias := net.JoinHostPort(p.HostAlias(id), strconv.Itoa(p.Access.Port))
		hosts = append(hosts, HostTrust{Node: target.Name, InstanceID: id, Fingerprint: fingerprint, KnownHostsLine: knownhosts.Line([]string{alias}, key)})
	}
	after, owner, err := store.Read(ctx, p.Compute)
	if err != nil {
		return nil, err
	}
	if owner != "" || after.Revision != r.Revision {
		return nil, errors.New("operation changed while collecting host trust; retry when idle")
	}
	if _, err := provision.RunningTargets(ctx, p.Compute, after, cloud); err != nil {
		return nil, err
	}
	return hosts, nil
}
