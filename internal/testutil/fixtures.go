package testutil

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/google/go-containerregistry/pkg/name"
)

func Network(t testing.TB) spec.Network {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	n, err := spec.Load(filepath.Join(filepath.Dir(file), "../../examples/devnet.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

type Inspector struct{ Fail string }

func (i Inspector) Inspect(_ context.Context, requested string, arches []string) (release.Image, error) {
	if i.Fail != "" && strings.Contains(requested, i.Fail) {
		return release.Image{}, fmt.Errorf("test registry unavailable")
	}
	ref, err := name.ParseReference(requested)
	if err != nil {
		return release.Image{}, err
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	image := release.Image{Requested: requested, Pinned: ref.Context().Digest(digest).Name()}
	for _, arch := range arches {
		image.Platforms = append(image.Platforms, release.Platform{OS: "linux", Architecture: arch, Digest: digest})
	}
	return image, nil
}

func Lock(t testing.TB, n spec.Network) release.Lock {
	t.Helper()
	l, err := release.Resolve(context.Background(), n, Inspector{}, time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return l
}
