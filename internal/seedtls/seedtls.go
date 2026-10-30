// Package seedtls adopts TLS on an existing DAPI bootstrap gateway. It does not
// enable mainnet in the network provisioner, managed upgrader, or reset paths.
package seedtls

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"time"

	"github.com/dashpay/dash-network-go/internal/transport"
	"github.com/google/go-containerregistry/pkg/name"
)

//go:embed worker.py
var worker string

//go:embed renew.py
var renew string

func Hash(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func RecipeDigest() string { return Hash([]string{worker, renew}) }

type Target struct {
	Name     string `json:"name"`
	Account  string `json:"account"`
	Region   string `json:"region"`
	Instance string `json:"instance"`
	Address  string `json:"address"`
	Hostname string `json:"hostname"`
	Gateway  string `json:"gateway"`
	Helper   string `json:"helper"`
}

var slug = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`)
var container = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var domain = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (t Target) Validate() error {
	ip := net.ParseIP(t.Address)
	if !slug.MatchString(t.Name) || !regexp.MustCompile(`^[0-9]{12}$`).MatchString(t.Account) || !regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`).MatchString(t.Region) || !regexp.MustCompile(`^i-[0-9a-f]{17}$`).MatchString(t.Instance) {
		return errors.New("explicit seed name and AWS account/region/instance required")
	}
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || len(t.Hostname) > 253 || !domain.MatchString(t.Hostname) || net.ParseIP(t.Hostname) != nil {
		return errors.New("seed requires public IPv4 and explicit DNS hostname")
	}
	if !container.MatchString(t.Gateway) || !container.MatchString(t.Helper) || t.Gateway == t.Helper {
		return errors.New("exact distinct gateway and helper containers required")
	}
	return nil
}
func (t Target) Alias() string {
	return fmt.Sprintf("%s.%s.%s.dashnet", t.Instance, t.Region, t.Account)
}

type Snapshot struct {
	Target      Target          `json:"target"`
	ObservedAt  time.Time       `json:"observedAt"`
	Baseline    json.RawMessage `json:"baseline"`
	Fingerprint string          `json:"fingerprint"`
}
type Plan struct {
	ID        string    `json:"id"`
	Recipe    string    `json:"recipe"`
	CreatedAt time.Time `json:"createdAt"`
	Snapshot  Snapshot  `json:"snapshot"`
	ACMEImage string    `json:"acmeImage"`
	Email     string    `json:"email"`
}

func Build(s Snapshot, image, email string, now time.Time) (Plan, error) {
	p := Plan{Recipe: RecipeDigest(), CreatedAt: now.UTC(), Snapshot: s, ACMEImage: image, Email: email}
	p.ID = p.identity()
	return p, p.Validate(now)
}
func (p Plan) identity() string { p.ID = ""; return Hash(p) }
func (p Plan) Validate(now time.Time) error {
	if err := p.Snapshot.Target.Validate(); err != nil {
		return err
	}
	if p.Recipe != RecipeDigest() || p.ID != p.identity() {
		return errors.New("plan/recipe identity mismatch")
	}
	if !digest.MatchString(p.Snapshot.Fingerprint) || !json.Valid(p.Snapshot.Baseline) || string(p.Snapshot.Baseline) == "null" {
		return errors.New("inspected baseline required")
	}
	if p.CreatedAt.After(now.Add(time.Minute)) || p.Snapshot.ObservedAt.After(p.CreatedAt.Add(time.Minute)) || p.Snapshot.ObservedAt.Before(now.Add(-24*time.Hour)) {
		return errors.New("fresh seed snapshot required (24 hours)")
	}
	ref, err := name.NewDigest(p.ACMEImage, name.StrictValidation)
	if err != nil || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(ref.DigestStr()) {
		return errors.New("ACME image must be pinned by sha256")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._%+\-]{1,64}@[A-Za-z0-9.\-]{1,190}\.[A-Za-z]{2,24}$`).MatchString(p.Email) {
		return errors.New("valid ACME contact required")
	}
	return nil
}

type SSH interface {
	Run(context.Context, transport.Endpoint, string, string) ([]byte, error)
}
type Request struct {
	Action      string `json:"action"`
	Target      Target `json:"target"`
	Plan        *Plan  `json:"plan,omitempty"`
	RenewSource string `json:"renewSource,omitempty"`
}
type Response struct {
	Baseline    json.RawMessage `json:"baseline,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	Status      json.RawMessage `json:"status,omitempty"`
	Error       string          `json:"error,omitempty"`
}

func Call(ctx context.Context, ssh SSH, q Request) (Response, error) {
	if ssh == nil {
		return Response{}, errors.New("authenticated SSH required")
	}
	if err := q.Target.Validate(); err != nil {
		return Response{}, err
	}
	if q.Action != "inspect" && q.Action != "doctor" && q.Action != "apply" && q.Action != "rollback" {
		return Response{}, errors.New("unsupported seed operation")
	}
	if q.Action == "apply" || q.Action == "rollback" {
		if q.Plan == nil || q.Plan.Snapshot.Target != q.Target {
			return Response{}, errors.New("exact seed plan required")
		}
		at := time.Now()
		if q.Action == "rollback" {
			at = q.Plan.CreatedAt
		}
		if err := q.Plan.Validate(at); err != nil {
			return Response{}, err
		}
		q.RenewSource = renew
	}
	var b bytes.Buffer
	z := zlib.NewWriter(&b)
	_, _ = z.Write([]byte(worker))
	_ = z.Close()
	command := "sudo -n /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/root /usr/bin/python3 -c \"import base64,zlib;exec(zlib.decompress(base64.b64decode('" + base64.StdEncoding.EncodeToString(b.Bytes()) + "')))\""
	body, _ := json.Marshal(q)
	raw, err := ssh.Run(ctx, transport.Endpoint{Address: q.Target.Address, Port: 22, HostAlias: q.Target.Alias()}, command, string(body))
	var r Response
	if json.Unmarshal(raw, &r) != nil {
		return r, errors.New("invalid seed response; raw output withheld")
	}
	if r.Error != "" {
		if regexp.MustCompile(`^[a-z0-9_-]{1,100}$`).MatchString(r.Error) {
			return r, fmt.Errorf("seed: %s", r.Error)
		}
		return r, errors.New("seed operation failed")
	}
	if err != nil {
		return r, err
	}
	return r, nil
}
