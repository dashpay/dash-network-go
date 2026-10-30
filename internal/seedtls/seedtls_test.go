package seedtls

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func target() Target {
	return Target{Name: "seed-1", Account: "123456789012", Region: "us-west-2", Instance: "i-00000000000000001", Address: "8.8.8.8", Hostname: "seed.example.org", Gateway: "existing-gateway-1", Helper: "existing-helper-1"}
}
func TestExactPlanAndScope(t *testing.T) {
	now := time.Now().UTC()
	s := Snapshot{Target: target(), ObservedAt: now, Baseline: json.RawMessage(`{"containers":{}}`), Fingerprint: strings.Repeat("a", 64)}
	p, e := Build(s, "docker.io/goacme/lego@sha256:"+strings.Repeat("b", 64), "ops@example.org", now)
	if e != nil {
		t.Fatal(e)
	}
	for name, mutate := range map[string]func(*Plan){
		"different hostname": func(q *Plan) { q.Snapshot.Target.Hostname = "other.example.org" },
		"different instance": func(q *Plan) { q.Snapshot.Target.Instance = "i-00000000000000002" },
		"different image":    func(q *Plan) { q.ACMEImage = "goacme/lego:latest" },
		"different recipe":   func(q *Plan) { q.Recipe = strings.Repeat("c", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			q := p
			mutate(&q)
			if q.Validate(now) == nil {
				t.Fatal("modified plan accepted")
			}
		})
	}
	if p.Validate(now.Add(25*time.Hour)) == nil {
		t.Fatal("stale snapshot accepted")
	}
	for _, bad := range []string{"x;touch /tmp/oops", "*.example.org", "127.0.0.1", "a..example.org", "UPPER.example.org"} {
		q := target()
		q.Hostname = bad
		if q.Validate() == nil {
			t.Fatalf("bad hostname accepted: %s", bad)
		}
	}
}
func TestNoUnpinnedACMEOrPrivateTarget(t *testing.T) {
	now := time.Now()
	s := Snapshot{Target: target(), ObservedAt: now, Baseline: json.RawMessage(`{}`), Fingerprint: strings.Repeat("a", 64)}
	for _, image := range []string{"lego:latest", "goacme/lego@sha256:abc", ""} {
		if _, err := Build(s, image, "ops@example.org", now); err == nil {
			t.Fatal("unpinned image accepted")
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "0.0.0.0", "::1"} {
		q := target()
		q.Address = ip
		if q.Validate() == nil {
			t.Fatal("nonpublic address accepted", ip)
		}
	}
}
