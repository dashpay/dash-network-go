package lifecycle

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
)

func TestDeployRendersEveryNodeAndPinsDashmateSidecarsOnce(t *testing.T) {
	p, r, s, f := setup(t)
	calls := 0
	r.Registry = fakeRegistry{calls: &calls}
	a, err := execute(t, p, r)
	if err != nil {
		t.Fatal(err)
	}
	pins := a.Deployment.Sidecars
	if len(pins) != 3 || calls != 3 {
		t.Fatal("sidecars not pinned once each", pins, calls)
	}
	requested := sidecarRequests("validator")
	for _, image := range pins {
		if image.Requested != requested[image.Service] {
			t.Fatal("pin is not dashmate's requested image", image)
		}
	}
	if tor := pins[0]; tor.Service != "core_tor" || !strings.HasSuffix(tor.Pinned, "@sha256:"+digest("tor")) {
		t.Fatal("dashmate's own Tor digest not kept", tor)
	}
	// Nothing starts before every node rendered and the pins are journaled.
	first := slices.IndexFunc(f.calls, func(q node.Request) bool { return q.Action == "core-start" })
	if first < 0 || slices.ContainsFunc(f.calls[:first], func(q node.Request) bool { return q.Action != "inspect" && q.Action != "render" }) {
		t.Fatal("a service started before rendering")
	}
	for _, q := range f.calls[first:] {
		if q.Action != "render" && len(q.Context.SidecarImages) != 3 {
			t.Fatal("request lacks the node's sidecar pins", q.Action, q.Context.SidecarImages)
		}
		for _, pin := range q.Context.SidecarImages {
			if !strings.Contains(pin, "@sha256:") {
				t.Fatal("sidecar pin not by digest", pin)
			}
		}
	}
	// A resume renders again but never resolves a tag again.
	s.record.Deployment.Phase, s.record.Deployment.Stage = "interrupted", "core-start"
	if _, err := execute(t, p, r); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal("resume resolved sidecar images again", calls)
	}
}

func TestNodesMustRenderTheSameDashmateRelease(t *testing.T) {
	p, r, _, f := setup(t)
	f.after = func(q node.Request, o *node.Observation) error {
		if q.Action == "render" && q.Target.Name == p.Validators()[3].Name {
			o.Render.Version = "4.2.0-beta.6"
		}
		return nil
	}
	if _, err := execute(t, p, r); err == nil || !strings.Contains(err.Error(), "renders with dashmate") {
		t.Fatal("mixed dashmate releases accepted", err)
	}
	for _, q := range f.calls {
		if q.Action == "core-start" {
			t.Fatal("Core started after a mixed render")
		}
	}
	p, r, _, f = setup(t)
	f.after = func(q node.Request, o *node.Observation) error {
		if q.Action == "render" && q.Target.Role == "wallet" {
			o.Render.Sidecars["core_tor"] = "osminogin/tor-simple:0.4.9.12"
		}
		return nil
	}
	if _, err := execute(t, p, r); err == nil || !strings.Contains(err.Error(), "different core_tor") {
		t.Fatal("nodes requesting different sidecars accepted", err)
	}
	p, r, _, _ = setup(t)
	r.Registry = nil
	if _, err := execute(t, p, r); err == nil || !strings.Contains(err.Error(), "registry access") {
		t.Fatal("sidecars left unpinned", err)
	}
}

func TestPlatformUpgradePinsNewSidecarsAndEnforcesStagedChanges(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	f.remote.calls = nil
	// The target release's dashmate requests a newer Tor, and reconfigures the
	// gateway although its image is unchanged.
	f.hook = func(q node.Request, o *node.Observation) error {
		if o.Render != nil {
			o.Render.Sidecars["core_tor"] = "osminogin/tor-simple:0.4.9.12"
			if !slices.Contains(o.Render.Changes, "gateway") {
				o.Render.Changes = append(o.Render.Changes, "gateway")
			}
		}
		if q.Action == "upgrade-apply" && q.Context.SidecarImages["core_tor"] == "" {
			return errors.New("apply without the new pins")
		}
		return nil
	}
	result, err := f.run(t, u)
	if err != nil {
		t.Fatal(err)
	}
	if callsFor(f.remote, "upgrade-stage") != 26 {
		t.Fatal("new sidecar pins were not staged again", callsFor(f.remote, "upgrade-stage"))
	}
	tor := result.Runtime.Sidecars[0]
	if tor.Service != "core_tor" || tor.Requested != "osminogin/tor-simple:0.4.9.12" {
		t.Fatal("runtime did not adopt the rollout's sidecars", result.Runtime.Sidecars)
	}
	for _, v := range f.plan.Validators() {
		if !slices.Contains(result.Upgrade.Changes[v.Name], "gateway") || !slices.Contains(result.Upgrade.Changes[v.Name], "drive_abci") {
			t.Fatal("staged changes not journaled", result.Upgrade.Changes[v.Name])
		}
	}
	// An apply that changes something else than it staged stops the rollout.
	f = upgradeSetup(t)
	u = f.change(t, "platform", "c")
	f.hook = func(q node.Request, o *node.Observation) error {
		if q.Action == "upgrade-apply" && o.Render != nil {
			o.Render.Changes = append(o.Render.Changes, "gateway")
		}
		return nil
	}
	if _, err := f.run(t, u); err == nil || !strings.Contains(err.Error(), "other than those staged") {
		t.Fatal("unstaged change accepted", err)
	}
}

func TestReconfiguredServicesAreExpectedOnlyWhereTheRolloutReached(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "tenderdash", "b")
	target := f.plan.Validators()[0]
	f.hook = func(q node.Request, o *node.Observation) error {
		if o.Render != nil {
			o.Render.Changes = append(o.Render.Changes, "gateway")
		}
		// The gateway is recreated with its unchanged image on reached nodes.
		if o.Platform != nil && f.store.record.Upgrade != nil {
			up := f.store.record.Upgrade
			if up.Completed[q.Target.Name] || up.CurrentNode == q.Target.Name && callsFor(f.remote, "upgrade-apply") > 0 {
				o.Platform.Containers["gateway"] = digest(q.Target.Name + "reconfigured")
			}
		}
		return nil
	}
	if _, err := f.run(t, u); err != nil {
		t.Fatal("staged reconfiguration refused", err)
	}
	// The same observation, for a node the rollout never reached, is drift.
	observed := f.runner.sample(context.Background(), f.plan, f.store.record, 0)
	if _, err := upgradeEvidence(f.plan, f.store.record, observed, ""); err != nil {
		t.Fatal("reached node's reconfiguration refused", err)
	}
	record := f.store.record
	record.Upgrade.CurrentNode, record.Upgrade.Completed[target.Name] = "", false
	record.Upgrade.Changes[target.Name] = nil
	if _, err := upgradeEvidence(f.plan, record, observed, ""); err == nil {
		t.Fatal("unstaged reconfiguration accepted")
	}
}

func TestSidecarJournalValidation(t *testing.T) {
	p, _, s, _ := setup(t)
	good := provision.Sidecars{{Service: "core_tor", Requested: "osminogin/tor-simple:0.4.9.11@sha256:" + digest("tor"),
		Pinned:    "index.docker.io/osminogin/tor-simple@sha256:" + digest("tor"),
		Platforms: []provision.SidecarPlatform{{Architecture: p.Targets[0].Architecture, Digest: "sha256:" + digest("x")}}}}
	if err := good.Validate(p.Bootstrap.Compute); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*provision.SidecarImage){
		func(x *provision.SidecarImage) { x.Service = "dashmate_helper" },
		func(x *provision.SidecarImage) { x.Pinned = "index.docker.io/other/tor@sha256:" + digest("tor") },
		func(x *provision.SidecarImage) {
			x.Pinned = "index.docker.io/osminogin/tor-simple@sha256:" + digest("other")
		},
		func(x *provision.SidecarImage) { x.Platforms = nil },
	} {
		bad := provision.Sidecars{good[0]}
		change(&bad[0])
		if bad.Validate(p.Bootstrap.Compute) == nil {
			t.Fatal("invalid sidecar pin accepted", bad[0])
		}
	}
	_ = s
	for ref, want := range map[string]string{
		"redis:alpine": "index.docker.io/library/redis:alpine",
		"osminogin/tor-simple:0.4.9.11@sha256:" + digest("tor"): "index.docker.io/osminogin/tor-simple@sha256:" + digest("tor"),
		"registry.example:5000/ratelimit:3fcc3609":              "registry.example:5000/ratelimit:3fcc3609",
	} {
		got, err := provision.Reference(ref)
		if err != nil || got.Name() != want {
			t.Fatal("reference", ref, got, err)
		}
	}
	if _, err := provision.Reference("Bad:tag@sha256:" + digest("x")); err == nil {
		t.Fatal("invalid tag accepted")
	}
}

func TestEveryCoreNodeCarriesTheDashmateHelper(t *testing.T) {
	p, _, _, _ := setup(t)
	for _, target := range p.Targets {
		var components []string
		for _, image := range target.Images {
			components = append(components, image.Component)
		}
		want := []string{"core", "dapi", "drive", "gateway", "helper", "tenderdash"}
		if target.Role != "validator" {
			want = []string{"core", "helper"}
		}
		if !slices.Equal(components, want) {
			t.Fatal("node renders without its release's dashmate", target.Name, components)
		}
	}
}

func TestIdlePlatformIsLiveWhileItsLastBlockIsRecent(t *testing.T) {
	for _, tc := range []struct {
		age     int64
		healthy bool
	}{{30, true}, {int64(PlatformIdleLimit / time.Second), true}, {int64(PlatformIdleLimit/time.Second) + 1, false}, {-1, false}} {
		p, r, s, f := setup(t)
		if _, err := execute(t, p, r); err != nil {
			t.Fatal(err)
		}
		// dashmate's Tenderdash makes an empty block only every three minutes:
		// between samples an idle chain does not advance.
		f.after = func(q node.Request, o *node.Observation) error {
			if o.Platform != nil {
				o.Platform.Height, o.Platform.DAPIHeight, o.Platform.BlockAge = 700, 700, tc.age
			}
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		health, err := r.Doctor(ctx, p, s.record)
		cancel()
		if err != nil || health.Healthy != tc.healthy {
			t.Fatal("idle Platform liveness", tc.age, health.Problems, err)
		}
		if health.ObservationWindow != "15s" {
			t.Fatal("observation window stretched", health.ObservationWindow)
		}
	}
}
