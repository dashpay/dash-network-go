package cli_test

import (
	"bytes"
	"context"
	"github.com/dashpay/dash-network-go/internal/cli"
	"strings"
	"testing"
)

func TestLifecycleCLIRequiresCompleteExplicitIntent(t *testing.T) {
	for _, command := range []string{"deployment-plan", "deploy", "doctor", "stop"} {
		if err := cli.Run(context.Background(), []string{command}, &bytes.Buffer{}, &bytes.Buffer{}, "test"); err == nil {
			t.Fatal("accepted incomplete intent", command)
		}
		if err := cli.Run(context.Background(), []string{command, "--help"}, &bytes.Buffer{}, &bytes.Buffer{}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"deployment-plan", "--protocol", "0"}, {"deployment-plan", "--protocol", "999"}, {"deploy", "--timeout", "0s"}, {"stop", "unexpected"}} {
		if cli.Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, "test") == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestObservationWindowRejectedBeforeAnyCloudOrHostAccess(t *testing.T) {
	for _, args := range [][]string{
		{"doctor", "--observation-window", "0s"},
		{"deploy", "--observation-window", "-1s"},
		{"doctor", "--observation-window", "2m", "--timeout", "1m"},
	} {
		err := cli.Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, "test")
		if err == nil || !strings.Contains(err.Error(), "--observation-window") {
			t.Fatal("invalid observation policy not rejected", args, err)
		}
	}
}
