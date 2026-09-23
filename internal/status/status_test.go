package status_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/dashpay/dash-network-go/internal/status"
)

func snapshot(now time.Time) inventory.Snapshot {
	return inventory.Snapshot{APIVersion: spec.Version, Kind: "Inventory", Network: spec.Metadata{Name: "devnet-demo", DisplayName: "Demo", Visibility: "public"}, Chain: spec.Chain{Type: "devnet", Generation: 1}, AccountID: "123456789012", Region: "us-west-2", ObservedAt: now, Source: "aws-ec2", ApplicationHealth: "unknown", Instances: []inventory.Instance{{ID: "i-private", Name: "internal-name", PrivateIP: "10.77.0.10", PublicIP: "203.0.113.1", State: "running"}}}
}

func TestPublicProjectionDoesNotExposeInventory(t *testing.T) {
	now := time.Now()
	s := snapshot(now)
	p, err := status.Project(s, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	for _, private := range []string{s.AccountID, s.Region, s.Instances[0].ID, s.Instances[0].Name, s.Instances[0].PrivateIP, s.Instances[0].PublicIP, "privateIp", "accountId", "instances"} {
		if strings.Contains(string(b), private) {
			t.Fatalf("public output leaked %q", private)
		}
	}
	if p.InstanceCount != 1 || p.Infrastructure["running"] != 1 || p.ApplicationHealth != "unknown" {
		t.Fatal("projection lost facts or invented health")
	}
	s.Network.Visibility = "private"
	if _, err := status.Project(s, now, time.Minute); err == nil {
		t.Fatal("private network exported publicly")
	}
}

func TestStalenessAndUnexpectedValues(t *testing.T) {
	now := time.Now()
	for _, observed := range []time.Time{now.Add(-10 * time.Minute), now.Add(2 * time.Minute)} {
		p, err := status.Project(snapshot(observed), now, time.Minute)
		if err != nil || !p.Stale {
			t.Fatalf("stale/future observation shown current: %v", err)
		}
	}
	s := snapshot(now)
	s.Instances[0].State = "sensitive-error-text"
	p, err := status.Project(s, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Infrastructure["sensitive-error-text"]; ok {
		t.Fatal("unknown source string escaped public allowlist")
	}
	s.ApplicationHealth = "healthy"
	if _, err := status.Project(s, now, time.Minute); err == nil {
		t.Fatal("unearned application health accepted")
	}
}
