package cli_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dashpay/dash-network-go/internal/cli"
	"github.com/dashpay/dash-network-go/internal/files"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

func TestMutationRequiresConcretePlanAcknowledgmentBeforeAWS(t *testing.T) {
	n := testutil.ProvisionNetwork(t)
	p, err := provision.Prepare(context.Background(), n, testutil.Identity{Account: n.AWS.AccountID}, &testutil.Cloud{Network: n})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err = files.WriteJSON(path, p); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"provision", "--plan", path},
		{"provision", "--plan", path, "--confirm", "wrong"},
		{"release-addresses", "--plan", path},
		{"release-addresses", "--plan", path, "--confirm", "wrong"},
		{"operation-unlock", "--plan", path},
		{"operation-unlock", "--plan", path, "--expected-owner", "old"},
		{"operation-unlock", "--plan", path, "--confirm-runner-stopped"},
	} {
		err = cli.Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, "test")
		if err == nil || !strings.Contains(err.Error(), "confirm") {
			t.Fatalf("confirmation boundary failed: %v", err)
		}
	}
	// Output collisions must be rejected before credential lookup or mutations.
	err = cli.Run(context.Background(), []string{"provision", "--plan", path, "--confirm", p.ID, "--out", path}, &bytes.Buffer{}, &bytes.Buffer{}, "test")
	if err == nil || !strings.Contains(err.Error(), "output already exists") {
		t.Fatal("existing evidence would be replaced", err)
	}
}
func TestProvisionCLIArguments(t *testing.T) {
	for _, args := range [][]string{{"provision"}, {"provision-plan"}, {"operation"}, {"operation-unlock"}, {"provision", "--timeout", "0s"}, {"operation", "unexpected"}, {"provision-plan", "--network", "../../examples/devnet.yaml"}} {
		if err := cli.Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, "test"); err == nil {
			t.Fatalf("accepted incomplete command %v", args)
		}
	}
	for _, cmd := range []string{"provision", "provision-plan", "operation", "operation-unlock", "release-addresses"} {
		if err := cli.Run(context.Background(), []string{cmd, "--help"}, &bytes.Buffer{}, &bytes.Buffer{}, "test"); err != nil {
			t.Fatal(err)
		}
	}
}
