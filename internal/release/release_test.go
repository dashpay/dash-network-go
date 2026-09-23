package release_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

func TestRejectsLockDrift(t *testing.T) {
	n := testutil.Network(t)
	original := testutil.Lock(t, n)
	data, _ := json.Marshal(original)
	for name, mutate := range map[string]func(*release.Lock){
		"network":     func(l *release.Lock) { l.Network = "devnet-another" },
		"generation":  func(l *release.Lock) { l.Generation++ },
		"spec":        func(l *release.Lock) { l.SpecHash = "changed" },
		"missing":     func(l *release.Lock) { l.Images = l.Images[1:] },
		"duplicate":   func(l *release.Lock) { l.Images[1] = l.Images[0] },
		"mutable pin": func(l *release.Lock) { l.Images[0].Pinned = "dashpay/dashd:23" },
		"other repository": func(l *release.Lock) {
			l.Images[0].Pinned = "example.org/other/image@sha256:" + strings.Repeat("a", 64)
		},
		"missing arch":          func(l *release.Lock) { l.Images[0].Platforms = nil },
		"wrong arch":            func(l *release.Lock) { l.Images[0].Platforms[0].Architecture = "amd64" },
		"bad digest":            func(l *release.Lock) { l.Images[0].Platforms[0].Digest = "sha256:no" },
		"unearned health claim": func(l *release.Lock) { l.Evidence = "runtime-verified" },
	} {
		t.Run(name, func(t *testing.T) {
			var l release.Lock
			if err := json.Unmarshal(data, &l); err != nil {
				t.Fatal(err)
			}
			mutate(&l)
			if l.Validate(n) == nil {
				t.Fatal("accepted mismatched lock")
			}
		})
	}
	n.Images["core"] = "docker.io/dashpay/dashd:next"
	if original.Validate(n) == nil {
		t.Fatal("changed intent accepted old lock")
	}
}

func TestResolutionFailureReturnsNoPartialLock(t *testing.T) {
	l, err := release.Resolve(context.Background(), testutil.Network(t), testutil.Inspector{Fail: "drive"}, time.Now())
	if err == nil || len(l.Images) != 0 {
		t.Fatal("partial release lock escaped failed resolution")
	}
}
