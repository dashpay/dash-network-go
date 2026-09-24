// Package transport executes bounded node commands over authenticated SSH.
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Endpoint struct {
	Address string
	Port    int
	// HostAlias is an account/region/instance identity, not a mutable IP address.
	HostAlias string
}

type SSH struct {
	config     *ssh.ClientConfig
	trustProbe ssh.PublicKey
}

// NewSSH never learns keys from the connection it is authenticating. The
// operator supplies a separately verified known_hosts file; missing/changed
// keys fail closed. Only the explicitly selected private key is offered.
func NewSSH(user, keyPath, hostsPath string) (*SSH, error) {
	f, err := os.Open(keyPath)
	if err != nil {
		return nil, errors.New("cannot open SSH identity file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("SSH identity must be a regular file with no group/other permissions")
	}
	key, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(key) > 64<<10 {
		return nil, errors.New("cannot read bounded SSH identity")
	}
	signer, err := ssh.ParsePrivateKey(key)
	for i := range key {
		key[i] = 0
	}
	if err != nil {
		return nil, errors.New("SSH identity must be a valid unencrypted private key; key contents are never reported")
	}
	check, err := knownhosts.New(hostsPath)
	if err != nil {
		return nil, errors.New("cannot load trusted SSH known_hosts file")
	}
	return &SSH{config: &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: check, Timeout: 15 * time.Second}, trustProbe: signer.PublicKey()}, nil
}

// Go's SSH defaults may select an unpinned ECDSA/RSA key before the separately
// pinned Ed25519 key. Negotiate only types present for this exact instance alias.
// The normal callback still verifies the actual server key bytes during the
// handshake. KeyError.Want is the known_hosts parser's matched key set, including
// hashed aliases; no keys are learned from the network or copied from other hosts.
func (s *SSH) hostAlgorithms(alias string) ([]string, error) {
	var keys []ssh.PublicKey
	err := s.config.HostKeyCallback(alias, hostAddress(alias), s.trustProbe)
	if err == nil {
		keys = append(keys, s.trustProbe)
	} else {
		var mismatch *knownhosts.KeyError
		if !errors.As(err, &mismatch) {
			return nil, errors.New("cannot resolve independently trusted instance host keys")
		}
		for _, key := range mismatch.Want {
			keys = append(keys, key.Key)
		}
	}
	allowed := map[string]bool{}
	for _, key := range keys {
		if key.Type() == ssh.KeyAlgoRSA {
			allowed[ssh.KeyAlgoRSASHA256] = true
			allowed[ssh.KeyAlgoRSASHA512] = true
		} else {
			allowed[key.Type()] = true
		}
	}
	var algorithms []string
	for _, algorithm := range ssh.SupportedAlgorithms().HostKeys {
		if allowed[algorithm] {
			algorithms = append(algorithms, algorithm)
		}
	}
	if len(algorithms) == 0 {
		return nil, errors.New("no supported host key pinned for this instance alias")
	}
	return algorithms, nil
}

type boundedOutput struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.buffer.Write(p)
	return n, nil // Drain excess so a noisy remote process cannot block cancellation.
}

// Run bounds handshake, execution and output, and closes the socket on context
// cancellation. Closing SSH is NOT proof that a remote process has stopped:
// callers must use a host-side lock and reconciliation before retrying.
func (s *SSH) Run(ctx context.Context, e Endpoint, command, stdin string) ([]byte, error) {
	if s == nil || s.config == nil || e.HostAlias == "" || net.ParseIP(e.Address) == nil || e.Port < 1 || e.Port > 65535 {
		return nil, errors.New("invalid SSH transport or endpoint")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("SSH commands require a context deadline")
	}
	address := net.JoinHostPort(e.Address, strconv.Itoa(e.Port))
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, errors.New("SSH connection failed (check routing and port)")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	handshakeDeadline := time.Now().Add(15 * time.Second)
	if deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	_ = conn.SetDeadline(handshakeDeadline)
	// Use a synthetic remote address for knownhosts: IP-based entries must not
	// silently substitute for the operator's instance-scoped trust pin.
	alias := net.JoinHostPort(e.HostAlias, strconv.Itoa(e.Port))
	config := *s.config
	config.HostKeyAlgorithms, err = s.hostAlgorithms(alias)
	if err != nil {
		return nil, err
	}
	config.HostKeyCallback = func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		return s.config.HostKeyCallback(hostname, hostAddress(alias), key)
	}
	remote, channels, requests, err := ssh.NewClientConn(conn, alias, &config)
	if err != nil {
		return nil, errors.New("SSH handshake failed (verify instance host key and login identity)")
	}
	_ = conn.SetDeadline(deadline)
	client := ssh.NewClient(remote, channels, requests)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return nil, errors.New("SSH session creation failed")
	}
	defer session.Close()
	stdout := &boundedOutput{limit: 64 << 10}
	session.Stdout = stdout
	session.Stderr = io.Discard // Never put remote banners, package logs or secrets in the journal.
	session.Stdin = bytes.NewBufferString(stdin)
	err = session.Run(command)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stdout.overflow {
		return nil, errors.New("SSH stdout exceeded 64 KiB; response refused")
	}
	data := append([]byte(nil), stdout.buffer.Bytes()...)
	if err != nil {
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			return data, fmt.Errorf("remote command exited %d; raw output withheld", exit.ExitStatus())
		}
		return nil, errors.New("SSH execution interrupted; remote outcome unknown")
	}
	return data, nil
}

type hostAddress string

func (h hostAddress) Network() string { return "tcp" }
func (h hostAddress) String() string  { return string(h) }
