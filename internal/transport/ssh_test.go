package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func key(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity")
	if err = os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	return signer, path
}
func server(t *testing.T, host, client ssh.Signer, handle func(ssh.Channel, string)) (Endpoint, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(port)
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(k.Marshal(), client.PublicKey().Marshal()) {
			return nil, io.EOF
		}
		return nil, nil
	}}
	config.AddHostKey(host)
	var calls atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				sc, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for incoming := range chans {
					if incoming.ChannelType() != "session" {
						_ = incoming.Reject(ssh.UnknownChannelType, "unsupported")
						continue
					}
					ch, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					for req := range requests {
						if req.Type != "exec" {
							_ = req.Reply(false, nil)
							continue
						}
						var payload struct{ Command string }
						_ = ssh.Unmarshal(req.Payload, &payload)
						_ = req.Reply(true, nil)
						calls.Add(1)
						handle(ch, payload.Command)
						_ = ch.Close()
						break
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); wg.Wait() })
	return Endpoint{Address: "127.0.0.1", Port: number, HostAlias: "i-00000001.us-east-1.123456789012.dashnet"}, &calls
}
func exit(ch ssh.Channel, status uint32) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
}
func knownFile(t *testing.T, e Endpoint, k ssh.PublicKey, kind string) string {
	t.Helper()
	alias := e.HostAlias
	if kind == "ip" {
		alias = e.Address
	}
	if kind == "missing" {
		alias = "another-host"
	}
	content := knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort(alias, strconv.Itoa(e.Port)))}, k) + "\n"
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestSSHRequiresInstanceScopedTrust(t *testing.T) {
	for _, kind := range []string{"trusted", "missing", "changed", "ip"} {
		t.Run(kind, func(t *testing.T) {
			host, _ := key(t)
			client, path := key(t)
			e, calls := server(t, host, client, func(ch ssh.Channel, command string) {
				input, _ := io.ReadAll(ch)
				_, _ = ch.Write(input)
				_, _ = ch.Stderr().Write([]byte("private diagnostic"))
				exit(ch, 0)
			})
			trusted := host.PublicKey()
			if kind == "changed" {
				other, _ := key(t)
				trusted = other.PublicKey()
			}
			remote, err := NewSSH("ubuntu", path, knownFile(t, e, trusted, kind))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, err := remote.Run(ctx, e, "safe-command", "expected")
			if kind == "trusted" {
				if err != nil || string(out) != "expected" || calls.Load() != 1 {
					t.Fatal("trusted execution failed", err, string(out))
				}
			} else if err == nil || calls.Load() != 0 {
				t.Fatal("untrusted SSH executed", err)
			}
		})
	}
}
func TestSSHOutputBoundsAndExitStatus(t *testing.T) {
	for _, kind := range []string{"overflow", "failure", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			host, _ := key(t)
			client, path := key(t)
			e, _ := server(t, host, client, func(ch ssh.Channel, command string) {
				_, _ = io.ReadAll(ch)
				switch kind {
				case "overflow":
					_, _ = ch.Write([]byte(strings.Repeat("x", 80<<10)))
					exit(ch, 0)
				case "failure":
					_, _ = ch.Write([]byte(`{"error":"host-lock"}`))
					_, _ = ch.Stderr().Write([]byte("SECRET"))
					exit(ch, 42)
				case "cancel":
					buf := make([]byte, 1)
					_, _ = ch.Read(buf)
					time.Sleep(150 * time.Millisecond)
				}
			})
			remote, err := NewSSH("ubuntu", path, knownFile(t, e, host.PublicKey(), "trusted"))
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Second
			if kind == "cancel" {
				deadline = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			out, err := remote.Run(ctx, e, "safe", "")
			if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(string(out), "SECRET") {
				t.Fatal("missing/private error", err)
			}
			if kind == "overflow" && !strings.Contains(err.Error(), "64 KiB") {
				t.Fatal(err)
			}
			if kind == "failure" && !strings.Contains(err.Error(), "42") {
				t.Fatal(err)
			}
		})
	}
}
func TestSSHKeyPermissionsAndDeadline(t *testing.T) {
	host, path := key(t)
	e := Endpoint{Address: "127.0.0.1", Port: 22, HostAlias: "host"}
	hosts := knownFile(t, e, host.PublicKey(), "trusted")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSSH("ubuntu", path, hosts); err == nil {
		t.Fatal("publicly readable private key accepted")
	}
	_ = os.Chmod(path, 0600)
	remote, err := NewSSH("ubuntu", path, hosts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = remote.Run(context.Background(), e, "safe", ""); err == nil {
		t.Fatal("unbounded SSH allowed")
	}
}
