package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestManagedCommandsRequireExplicitScopeBeforeAWS(t *testing.T) {
	for _, command := range []string{"managed-import", "managed-enroll", "managed-plan", "managed-deploy", "managed-upgrade", "managed-doctor", "managed-operation", "managed-unlock"} {
		t.Run(command, func(t *testing.T) {
			var out, errout bytes.Buffer
			if e := Run(context.Background(), []string{command}, &out, &errout, "test"); e == nil {
				t.Fatal("accepted implicit intent")
			}
			if e := Run(context.Background(), []string{command, "--help"}, &out, &errout, "test"); e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(errout.String(), "Usage") {
				t.Fatal("no emergency help")
			}
		})
	}
}
