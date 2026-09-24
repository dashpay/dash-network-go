// Package node provides a small, typed host protocol over authenticated SSH.
// Secrets are generated/persisted on hosts; only public observations return.
package node

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/transport"
)

//go:embed worker.py
var recipe string

//go:embed observer.py
var observer string

func RecipeDigest() string { s := sha256.Sum256([]byte(recipe)); return hex.EncodeToString(s[:]) }

func ObservationDigest() string {
	s := sha256.Sum256([]byte(observer))
	return hex.EncodeToString(s[:])
}

func workerScript(action string) string {
	if action == "inspect" || action == "core-status" || action == "platform-status" {
		// Do not execute the original entry point until the observation-only
		// capability guard and compatible RPC reader have been installed.
		return "__name__ = 'dashnet_observation'\n" + recipe + "\n" + observer + "\nmain()\n"
	}
	return recipe
}

type Target struct {
	Name         string            `json:"name"`
	Role         string            `json:"role"`
	Architecture string            `json:"architecture"`
	InstanceID   string            `json:"instanceId"`
	SSHAddress   string            `json:"sshAddress"`
	PeerAddress  string            `json:"peerAddress"`
	Images       []bootstrap.Image `json:"images"`
}

var resourceID = regexp.MustCompile(`^i-([0-9a-f]{8}|[0-9a-f]{17})$`)
var nodeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,66}$`)

func (t Target) Validate() error {
	if !nodeName.MatchString(t.Name) || !resourceID.MatchString(t.InstanceID) {
		return errors.New("invalid node name or instance ID")
	}
	for _, s := range []string{t.SSHAddress, t.PeerAddress} {
		ip := net.ParseIP(s)
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("node requires usable IPv4 addresses")
		}
	}
	return nil
}

type Ports struct {
	CoreP2P     int `json:"coreP2P"`
	CoreRPC     int `json:"coreRPC"`
	CoreZMQ     int `json:"coreZMQ"`
	PlatformP2P int `json:"platformP2P"`
	PlatformRPC int `json:"platformRPC"`
	DriveABCI   int `json:"driveABCI"`
	DriveGRPC   int `json:"driveGRPC"`
	DAPIGRPC    int `json:"dapiGRPC"`
	DAPIJSON    int `json:"dapiJSON"`
	Gateway     int `json:"gateway"`
}

var DefaultPorts = Ports{20001, 20002, 29998, 26656, 26657, 26658, 26670, 3010, 3009, 1443}

type Context struct {
	PlanID                 string   `json:"planId"`
	ComputePlanID          string   `json:"computePlanId"`
	BootstrapID            string   `json:"bootstrapId"`
	Network                string   `json:"network"`
	CoreNetwork            string   `json:"coreNetwork"`
	PlatformChainID        string   `json:"platformChainId"`
	GenesisTime            string   `json:"genesisTime"`
	InitialProtocolVersion uint32   `json:"initialProtocolVersion"`
	MiningIntervalSeconds  int      `json:"miningIntervalSeconds"`
	MiningNodeName         string   `json:"miningNodeName"`
	CorePeers              []string `json:"corePeers"`
	Ports                  Ports    `json:"ports"`
}
type Peer struct {
	Name              string `json:"name"`
	Address           string `json:"address"`
	NodeID            string `json:"nodeId"`
	OperatorPublicKey string `json:"operatorPublicKey"`
	ProTxHash         string `json:"proTxHash,omitempty"`
}
type Request struct {
	Context               Context `json:"context"`
	Target                Target  `json:"target"`
	Action                string  `json:"action"`
	SporkAddress          string  `json:"sporkAddress,omitempty"`
	PayoutAddress         string  `json:"payoutAddress,omitempty"`
	Registration          *Peer   `json:"registration,omitempty"`
	Peers                 []Peer  `json:"peers,omitempty"`
	GenesisCoreHeight     int64   `json:"genesisCoreHeight,omitempty"`
	RequiredBalance       int64   `json:"requiredBalance,omitempty"`
	RequiredConfirmations int     `json:"requiredConfirmations,omitempty"`
	ReferenceHeight       int64   `json:"referenceHeight,omitempty"`
	// Transient input candidates. They are never journaled or printed, and the
	// worker refuses to replace existing identity files on resume.
	NodePrivateKey string `json:"nodePrivateKey,omitempty"`
	TLSCertificate string `json:"tlsCertificate,omitempty"`
	TLSPrivateKey  string `json:"tlsPrivateKey,omitempty"`
}
type Mining struct {
	Running     bool   `json:"running"`
	ContainerID string `json:"containerId"`
	Restarts    int    `json:"restarts"`
}
type Core struct {
	Mining          *Mining        `json:"mining,omitempty"`
	Genesis         string         `json:"genesis"`
	Height          int64          `json:"height"`
	Headers         int64          `json:"headers"`
	Synced          bool           `json:"synced"`
	IBD             bool           `json:"ibd"`
	Peers           int            `json:"peers"`
	ContainerID     string         `json:"containerId"`
	Restarts        int            `json:"restarts"`
	ConfigSHA256    string         `json:"configSha256"`
	MasternodeState string         `json:"masternodeState,omitempty"`
	ProTxHash       string         `json:"proTxHash,omitempty"`
	ChainLockHeight int64          `json:"chainLockHeight"`
	Quorums         map[string]int `json:"quorums"`
}
type Platform struct {
	Height             int64             `json:"height"`
	DAPIHeight         int64             `json:"dapiHeight"`
	CatchingUp         bool              `json:"catchingUp"`
	ChainID            string            `json:"chainId"`
	NodeID             string            `json:"nodeId"`
	ProTxHash          string            `json:"proTxHash"`
	DriveVersion       string            `json:"driveVersion"`
	ReferenceBlockHash string            `json:"referenceBlockHash,omitempty"`
	Containers         map[string]string `json:"containers"`
	Restarts           map[string]int    `json:"restarts"`
}
type Observation struct {
	InstanceID        string    `json:"instanceId"`
	PlanID            string    `json:"planId"`
	Action            string    `json:"action"`
	Error             string    `json:"error,omitempty"`
	Core              *Core     `json:"core,omitempty"`
	Platform          *Platform `json:"platform,omitempty"`
	OperatorPublicKey string    `json:"operatorPublicKey,omitempty"`
	PlatformNodeID    string    `json:"platformNodeId,omitempty"`
	PayoutAddress     string    `json:"payoutAddress,omitempty"`
	SporkAddress      string    `json:"sporkAddress,omitempty"`
	ProTxHash         string    `json:"proTxHash,omitempty"`
	Confirmations     int       `json:"confirmations,omitempty"`
	Balance           int64     `json:"balance,omitempty"`
}
type Backend interface {
	Call(context.Context, Request) (Observation, error)
}
type Remote struct {
	SSH             bootstrap.Remote
	Access          bootstrap.Access
	Account, Region string
}

var actions = map[string]bool{"inspect": true, "core-start": true, "core-status": true, "identity": true, "wallet": true, "core-finalize": true, "fund": true, "register": true, "activate": true, "mine-start": true, "platform-start": true, "platform-status": true, "stop": true}
var diagnostic = regexp.MustCompile(`^[a-z0-9:_-]{1,120}$`)

func (r Remote) Call(ctx context.Context, q Request) (Observation, error) {
	if r.SSH == nil {
		return Observation{}, errors.New("authenticated SSH required")
	}
	if _, ok := ctx.Deadline(); !ok {
		return Observation{}, errors.New("node operation requires a deadline")
	}
	if !actions[q.Action] {
		return Observation{}, errors.New("unsupported node action")
	}
	if err := q.Target.Validate(); err != nil {
		return Observation{}, err
	}
	if q.Action == "identity" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return Observation{}, err
		}
		q.NodePrivateKey = base64.StdEncoding.EncodeToString(priv)
		cert, key, err := certificate(q.Target)
		if err != nil {
			return Observation{}, err
		}
		q.TLSCertificate = cert
		q.TLSPrivateKey = key
	}
	body, err := json.Marshal(q)
	if err != nil {
		return Observation{}, err
	}
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	_, _ = z.Write([]byte(workerScript(q.Action)))
	_ = z.Close()
	command := "/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/root /usr/bin/python3 -c \"import base64,zlib;exec(zlib.decompress(base64.b64decode('" + base64.StdEncoding.EncodeToString(compressed.Bytes()) + "')))\""
	if r.Access.User != "root" {
		command = "sudo -n -- " + command
	}
	endpoint := transport.Endpoint{Address: q.Target.SSHAddress, Port: r.Access.Port, HostAlias: fmt.Sprintf("%s.%s.%s.dashnet", q.Target.InstanceID, r.Region, r.Account)}
	data, runErr := r.SSH.Run(ctx, endpoint, command, string(body))
	var o Observation
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	decodeErr := d.Decode(&o)
	var extra any
	if decodeErr == nil && !errors.Is(d.Decode(&extra), io.EOF) {
		decodeErr = errors.New("extra node output")
	}
	if runErr != nil {
		if decodeErr == nil && diagnostic.MatchString(o.Error) {
			return Observation{}, fmt.Errorf("node %s: %w", o.Error, runErr)
		}
		return Observation{}, runErr
	}
	if decodeErr != nil || o.Error != "" || o.InstanceID != q.Target.InstanceID || o.PlanID != q.Context.PlanID || o.Action != q.Action {
		return Observation{}, errors.New("invalid/mismatched node observation; raw output withheld")
	}
	return o, nil
}
func certificate(t Target) (string, string, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	c := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: t.Name}, DNSNames: []string{t.Name, "localhost"}, IPAddresses: []net.IP{net.ParseIP(t.PeerAddress), net.ParseIP(t.SSHAddress), net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, c, c, pub, key)
	if err != nil {
		return "", "", err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})), nil
}
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(s) > 4096 {
		s = s[:4096]
	}
	return s
}
