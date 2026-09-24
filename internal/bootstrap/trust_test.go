package bootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/dashpay/dash-network-go/internal/testutil"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testConsole(key ssh.PublicKey) string {
	return "private-console-sentinel\n-----BEGIN SSH HOST KEY KEYS-----\n" + string(ssh.MarshalAuthorizedKey(key)) + "-----END SSH HOST KEY KEYS-----\n"
}

func TestConsoleKeyParsingNeverUsesUnscopedOrAmbiguousKeys(t *testing.T) {
	key := testHostKey(t)
	valid := testConsole(key)
	parsed, err := ConsoleHostKey(base64.StdEncoding.EncodeToString([]byte(valid)))
	if err != nil || ssh.FingerprintSHA256(parsed) != ssh.FingerprintSHA256(key) {
		t.Fatal("valid cloud-init public identity rejected", err)
	}
	for _, raw := range []string{
		string(ssh.MarshalAuthorizedKey(key)),
		strings.ReplaceAll(valid, "-----END SSH HOST KEY KEYS-----", ""),
		valid + valid,
		strings.ReplaceAll(valid, string(ssh.MarshalAuthorizedKey(key)), string(ssh.MarshalAuthorizedKey(key))+string(ssh.MarshalAuthorizedKey(key))),
		strings.ReplaceAll(valid, string(ssh.MarshalAuthorizedKey(key)), "ssh-ed25519 invalid\n"),
		strings.ReplaceAll(valid, string(ssh.MarshalAuthorizedKey(key)), ""),
		"-----END SSH HOST KEY KEYS-----\n" + valid,
	} {
		_, err := ConsoleHostKey(base64.StdEncoding.EncodeToString([]byte(raw)))
		if err == nil {
			t.Fatal("ambiguous or unscoped host identity accepted")
		}
		if strings.Contains(err.Error(), "private-console-sentinel") {
			t.Fatal("console data leaked through diagnostic")
		}
	}
	for _, encoded := range []string{"", "not-base64", strings.Repeat("a", 1<<20+1)} {
		if _, err := ConsoleHostKey(encoded); err == nil {
			t.Fatal("unbounded or malformed console accepted")
		}
	}
}

type testConsoles struct {
	keys      map[string]ssh.PublicKey
	timestamp time.Time
	calls     int
	hook      func(*ec2.GetConsoleOutputOutput)
}

func (f *testConsoles) GetConsoleOutput(_ context.Context, input *ec2.GetConsoleOutputInput, _ ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error) {
	f.calls++
	key := f.keys[aws.ToString(input.InstanceId)]
	if key == nil || !aws.ToBool(input.Latest) {
		return nil, errors.New("unexpected console request")
	}
	out := &ec2.GetConsoleOutputOutput{InstanceId: input.InstanceId, Timestamp: &f.timestamp, Output: aws.String(base64.StdEncoding.EncodeToString([]byte(testConsole(key))))}
	if f.hook != nil {
		f.hook(out)
	}
	return out, nil
}

func TestCollectAllInstanceScopedKeysWithoutMutations(t *testing.T) {
	for _, port := range []int{22, 2222} {
		p, cloud, store, _ := setup(t)
		var err error
		p, err = Build(p.Compute, p.Release, Access{User: "ubuntu", Port: port, Address: "private"})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		f := &testConsoles{keys: map[string]ssh.PublicKey{}, timestamp: now}
		for i := range cloud.Instances {
			cloud.Instances[i].LaunchTime = aws.Time(now.Add(-time.Minute))
			f.keys[aws.ToString(cloud.Instances[i].InstanceId)] = testHostKey(t)
		}
		revision, launches := store.record.Revision, len(cloud.Requests)
		hosts, err := CollectTrust(context.Background(), p, testutil.Identity{Account: p.Compute.Network.AWS.AccountID}, cloud, f, store, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(hosts) != len(p.Targets) || store.record.Revision != revision || store.owner != "" || len(cloud.Requests) != launches {
			t.Fatal("collection changed cloud/journal or omitted targets")
		}
		path := filepath.Join(t.TempDir(), "known_hosts")
		var data string
		for _, h := range hosts {
			data += h.KnownHostsLine + "\n"
		}
		if err = os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		check, err := knownhosts.New(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hosts {
			address := net.JoinHostPort(p.HostAlias(h.InstanceID), strconv.Itoa(port))
			if err = check(address, &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: port}, f.keys[h.InstanceID]); err != nil {
				t.Fatal(err)
			}
			if err = check(address, &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: port}, testHostKey(t)); err == nil {
				t.Fatal("changed host key accepted")
			}
		}
	}
}

func TestTrustFailsClosedWithoutPartialOutput(t *testing.T) {
	for _, failure := range []string{"wrong-account", "active-runner", "stale-console", "wrong-instance", "missing-time", "future-time", "cloned-key", "claim-during-read", "journal-during-read", "cloud-during-read", "missing-key"} {
		t.Run(failure, func(t *testing.T) {
			p, cloud, store, _ := setup(t)
			now := time.Now().UTC()
			f := &testConsoles{keys: map[string]ssh.PublicKey{}, timestamp: now}
			for i := range cloud.Instances {
				cloud.Instances[i].LaunchTime = aws.Time(now.Add(-time.Minute))
				f.keys[aws.ToString(cloud.Instances[i].InstanceId)] = testHostKey(t)
			}
			account := p.Compute.Network.AWS.AccountID
			switch failure {
			case "wrong-account":
				account = "000000000000"
			case "active-runner":
				store.owner = "busy"
			case "stale-console":
				f.timestamp = now.Add(-time.Hour)
			case "wrong-instance":
				f.hook = func(o *ec2.GetConsoleOutputOutput) { o.InstanceId = aws.String("i-ffffffff") }
			case "missing-time":
				f.hook = func(o *ec2.GetConsoleOutputOutput) { o.Timestamp = nil }
			case "future-time":
				f.timestamp = now.Add(time.Hour)
			case "cloned-key":
				key := testHostKey(t)
				for id := range f.keys {
					f.keys[id] = key
				}
			case "claim-during-read":
				f.hook = func(_ *ec2.GetConsoleOutputOutput) { store.owner = "other" }
			case "journal-during-read":
				f.hook = func(_ *ec2.GetConsoleOutputOutput) { store.record.Revision++ }
			case "cloud-during-read":
				f.hook = func(_ *ec2.GetConsoleOutputOutput) { cloud.Instances[0].InstanceId = aws.String("i-ffffffff") }
			case "missing-key":
				f.hook = func(o *ec2.GetConsoleOutputOutput) {
					if f.calls == len(p.Targets) {
						o.Output = aws.String("")
					}
				}
			}
			hosts, err := CollectTrust(context.Background(), p, testutil.Identity{Account: account}, cloud, f, store, now)
			if err == nil || len(hosts) != 0 {
				t.Fatal("failed scope emitted partial trust", err)
			}
			if (failure == "wrong-account" || failure == "active-runner") && f.calls != 0 {
				t.Fatal("console read before scope/claim validation")
			}
		})
	}
}
