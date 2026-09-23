package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dashpay/dash-network-go/internal/cli"
	"github.com/dashpay/dash-network-go/internal/files"
	"github.com/dashpay/dash-network-go/internal/plan"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

func TestOfflineCLIPlanning(t *testing.T) {
	n := testutil.Network(t)
	lock := testutil.Lock(t, n)
	path := filepath.Join(t.TempDir(), "release.json")
	if err := files.WriteJSON(path, lock); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	err := cli.Run(context.Background(), []string{"plan", "--network", "../../examples/devnet.yaml", "--lock", path, "--operation", "upgrade", "--scope", "platform"}, &out, &errout, "test")
	if err != nil {
		t.Fatal(err)
	}
	var p plan.Plan
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Network != n.Metadata.Name || p.Executable || p.Scope != "platform" {
		t.Fatal("unexpected CLI plan")
	}
}

func TestCLIRejectsImplicitOrUnimplementedActions(t *testing.T) {
	for _, args := range [][]string{{"apply"}, {"destroy"}, {"validate"}, {"validate", "--network", "../../examples/devnet.yaml", "unexpected"}, {"plan", "--network", "../../examples/devnet.yaml"}, {"status"}, {"validate", "--network", "../../examples/devnet.yaml", "--timeout", "0s"}} {
		if err := cli.Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, "test"); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var out bytes.Buffer
	if err := cli.Run(context.Background(), []string{"help"}, &out, &bytes.Buffer{}, "test"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "read-only") {
		t.Fatal("scope not stated in help")
	}
}

func TestNoArtifactWrittenOnInvalidPlan(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "plan.json")
	err := cli.Run(context.Background(), []string{"plan", "--network", "../../examples/devnet.yaml", "--lock", filepath.Join(dir, "missing"), "--out", output}, &bytes.Buffer{}, &bytes.Buffer{}, "test")
	if err == nil {
		t.Fatal("missing lock accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed command published an artifact")
	}
}
