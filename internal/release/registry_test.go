package release_test

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func imageFor(t *testing.T, arch string) v1.Image {
	t.Helper()
	i, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{Architecture: arch, OS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestRegistryLocksIndexAndVerifiesArchitecture(t *testing.T) {
	s := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer s.Close()
	ref, err := name.NewTag(strings.TrimPrefix(s.URL, "http://") + "/dash/node:beta")
	if err != nil {
		t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: imageFor(t, "amd64"), Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: imageFor(t, "arm64"), Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}})
	if err := remote.WriteIndex(ref, index); err != nil {
		t.Fatal(err)
	}
	r := release.Registry{}
	locked, err := r.Inspect(context.Background(), ref.Name(), []string{"amd64", "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locked.Platforms) != 2 || !strings.Contains(locked.Pinned, "@sha256:") {
		t.Fatal("multiarch lock incomplete")
	}
	if err := remote.Write(ref, imageFor(t, "amd64")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Inspect(context.Background(), ref.Name(), []string{"arm64"}); err == nil {
		t.Fatal("wrong-architecture image accepted")
	}
	// The moving tag changed, but the original digest still resolves identically.
	again, err := r.Inspect(context.Background(), locked.Pinned, []string{"amd64", "arm64"})
	if err != nil || again.Pinned != locked.Pinned {
		t.Fatalf("immutable lock changed: %v", err)
	}
}

func TestRegistryRejectsFalsePlatformAdvertisement(t *testing.T) {
	s := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer s.Close()
	ref, _ := name.NewTag(strings.TrimPrefix(s.URL, "http://") + "/dash/node:bad")
	idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: imageFor(t, "amd64"), Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}})
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	if _, err := (release.Registry{}).Inspect(context.Background(), ref.Name(), []string{"arm64"}); err == nil {
		t.Fatal("trusted index over actual image architecture")
	}
}
