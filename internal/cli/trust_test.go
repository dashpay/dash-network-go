package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dashpay/dash-network-go/internal/cli"
)

func TestHostTrustRequiresExplicitArtifactsAndCannotReplaceTrust(t *testing.T) {
	for _, args := range [][]string{
		{"host-trust"},
		{"host-trust", "--bootstrap-plan", "missing"},
		{"host-trust", "--bootstrap-plan", "missing", "--out", "missing", "--timeout", "0s"},
	} {
		if err := cli.Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, "test"); err == nil {
			t.Fatal("incomplete trust request accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte("trusted-original"), 0600); err != nil {
		t.Fatal(err)
	}
	err := cli.Run(context.Background(), []string{"host-trust", "--bootstrap-plan", "missing", "--out", path}, &bytes.Buffer{}, &bytes.Buffer{}, "test")
	if err == nil || !strings.Contains(err.Error(), "existing host trust is never replaced") {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "trusted-original" {
		t.Fatal("overwrote host trust")
	}
	if err := cli.Run(context.Background(), []string{"host-trust", "--help"}, &bytes.Buffer{}, &bytes.Buffer{}, "test"); err != nil {
		t.Fatal(err)
	}
}
