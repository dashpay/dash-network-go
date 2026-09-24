package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/cli"
	"github.com/dashpay/dash-network-go/internal/files"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

func TestBootstrapOfflinePlanAndMutationBoundary(t *testing.T) {
	n := testutil.ProvisionNetwork(t)
	cloud := &testutil.Cloud{Network: n}
	p, err := provision.Prepare(context.Background(), n, testutil.Identity{Account: n.AWS.AccountID}, cloud)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	computePath := filepath.Join(dir, "compute.json")
	lockPath := filepath.Join(dir, "lock.json")
	if err = files.WriteJSON(computePath, p); err != nil {
		t.Fatal(err)
	}
	if err = files.WriteJSON(lockPath, testutil.Lock(t, n)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = cli.Run(context.Background(), []string{"bootstrap-plan", "--compute-plan", computePath, "--lock", lockPath}, &out, &bytes.Buffer{}, "test"); err != nil {
		t.Fatal(err)
	}
	var b bootstrap.Plan
	if err = json.Unmarshal(out.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if err = b.Validate(); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "bootstrap.json")
	if err = files.WriteJSON(planPath, b); err != nil {
		t.Fatal(err)
	}
	for _, confirm := range []string{"", "wrong"} {
		err = cli.Run(context.Background(), []string{"bootstrap", "--plan", planPath, "--confirm", confirm}, &bytes.Buffer{}, &bytes.Buffer{}, "test")
		if err == nil || !strings.Contains(err.Error(), "--confirm") {
			t.Fatal("mutation boundary failed", err)
		}
	}
	err = cli.Run(context.Background(), []string{"bootstrap", "--plan", planPath, "--confirm", b.ID}, &bytes.Buffer{}, &bytes.Buffer{}, "test")
	if err == nil || !strings.Contains(err.Error(), "known-hosts") {
		t.Fatal("host trust optional", err)
	}
	err = cli.Run(context.Background(), []string{"bootstrap", "--plan", planPath, "--confirm", b.ID, "--ssh-key", "unused", "--known-hosts", "unused", "--out", planPath}, &bytes.Buffer{}, &bytes.Buffer{}, "test")
	if err == nil || !strings.Contains(err.Error(), "output already exists") {
		t.Fatal("output overwritten", err)
	}
}
func TestBootstrapCLIArguments(t *testing.T) {
	for _, args := range [][]string{{"bootstrap"}, {"bootstrap-plan"}, {"bootstrap", "--timeout", "0s"}, {"bootstrap-plan", "unexpected"}} {
		if err := cli.Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, "test"); err == nil {
			t.Fatal("accepted incomplete command", args)
		}
	}
	for _, command := range []string{"bootstrap", "bootstrap-plan"} {
		if err := cli.Run(context.Background(), []string{command, "--help"}, &bytes.Buffer{}, &bytes.Buffer{}, "test"); err != nil {
			t.Fatal(err)
		}
	}
}
