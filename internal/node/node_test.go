package node

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/transport"
)

type fakeSSH struct {
	inspect func(transport.Endpoint, string, string) ([]byte, error)
}

func (f fakeSSH) Run(_ context.Context, e transport.Endpoint, c, s string) ([]byte, error) {
	return f.inspect(e, c, s)
}
func target() Target {
	return Target{Name: "validator-1", Role: "validator", Architecture: "arm64", InstanceID: "i-12345678", SSHAddress: "10.0.0.2", PeerAddress: "10.0.0.2"}
}

func TestObservationEntryPointCannotExecuteMutation(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python worker contract runs in CI")
	}
	v := target()
	v.Images = []bootstrap.Image{}
	q := Request{Target: v, Action: "core-start", Context: Context{PlanID: strings.Repeat("a", 64), ComputePlanID: strings.Repeat("b", 64), Ports: DefaultPorts}}
	input, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(python, "-c", workerScript("platform-status"))
	command.Stdin = strings.NewReader(string(input))
	out, err := command.Output()
	if err == nil || !strings.Contains(string(out), "preflight:observation-only") {
		t.Fatal("observation adapter reached the original mutable entry point", string(out), err)
	}
	if workerScript("core-start") != recipe {
		t.Fatal("mutation recipe was altered by observation dispatch")
	}
}
func TestPrivateIdentityOnlyOnStdin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	q := Request{Target: target(), Action: "identity", Context: Context{PlanID: strings.Repeat("a", 64)}}
	r := Remote{Access: bootstrap.Access{User: "ubuntu", Port: 22}, Account: "123456789012", Region: "us-east-1", SSH: fakeSSH{inspect: func(e transport.Endpoint, command, stdin string) ([]byte, error) {
		var got Request
		if err := json.Unmarshal([]byte(stdin), &got); err != nil {
			t.Fatal(err)
		}
		raw, err := base64.StdEncoding.DecodeString(got.NodePrivateKey)
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			t.Fatal("invalid node key")
		}
		if !strings.HasPrefix(command, "sudo -n -- ") || strings.Contains(command, got.NodePrivateKey) || strings.Contains(command, got.TLSPrivateKey) {
			t.Fatal("private material in command")
		}
		if e.HostAlias != "i-12345678.us-east-1.123456789012.dashnet" {
			t.Fatal("lost instance host trust")
		}
		certBlock, _ := pem.Decode([]byte(got.TLSCertificate))
		cert, err := x509.ParseCertificate(certBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if err = cert.VerifyHostname("127.0.0.1"); err != nil {
			t.Fatal(err)
		}
		if err = cert.VerifyHostname("10.0.0.2"); err != nil {
			t.Fatal(err)
		}
		pk, _ := pem.Decode([]byte(got.TLSPrivateKey))
		if _, err = x509.ParsePKCS8PrivateKey(pk.Bytes); err != nil {
			t.Fatal(err)
		}
		id := sha256.Sum256(raw[32:])
		return json.Marshal(Observation{InstanceID: q.Target.InstanceID, PlanID: q.Context.PlanID, Action: q.Action, PlatformNodeID: hex.EncodeToString(id[:20])})
	}}}
	if _, err := r.Call(ctx, q); err != nil {
		t.Fatal(err)
	}
}
func TestRemoteRejectsIdentityMismatchAndRawErrorOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	q := Request{Target: target(), Action: "inspect", Context: Context{PlanID: strings.Repeat("a", 64)}}
	for _, raw := range []string{`{"privateKey":"DO-NOT-PRINT"}`, `{"instanceId":"i-99999999","planId":"wrong","action":"inspect"}`, `{"error":"DO-NOT-PRINT-secret"}`, `{} {}`} {
		r := Remote{SSH: fakeSSH{inspect: func(transport.Endpoint, string, string) ([]byte, error) { return []byte(raw), nil }}}
		_, err := r.Call(ctx, q)
		if err == nil || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal("raw remote output escaped", err)
		}
	}
	r := Remote{SSH: fakeSSH{inspect: func(transport.Endpoint, string, string) ([]byte, error) {
		return []byte(`{"error":"registration-submit:rpc-protx:-5"}`), errors.New("SSH exit 1")
	}}}
	_, err := r.Call(ctx, q)
	if err == nil || !strings.Contains(err.Error(), "registration-submit") {
		t.Fatal("sanitized stage lost")
	}
	for _, ip := range []string{"127.0.0.1", "169.254.169.254", "0.0.0.0", "::1"} {
		v := target()
		v.SSHAddress = ip
		if v.Validate() == nil {
			t.Fatal("unsafe address accepted", net.ParseIP(ip))
		}
	}
}
