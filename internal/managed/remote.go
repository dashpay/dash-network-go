package managed

import (
	"bytes"
	"compress/zlib"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/transport"
)

//go:embed worker.py
var worker string

func RecipeDigest() string { return hash(worker) }

type Request struct {
	Fleet           Fleet             `json:"fleet"`
	FleetID         string            `json:"fleetId"`
	Target          Target            `json:"target"`
	Action          string            `json:"action"`
	Expected        *Observation      `json:"expected,omitempty"`
	Pins            map[string]string `json:"pins,omitempty"`
	Operation       string            `json:"operation,omitempty"`
	OperationID     string            `json:"operationId,omitempty"`
	ReferenceHeight int64             `json:"referenceHeight,omitempty"`
}
type Backend interface {
	Call(context.Context, Request) (Observation, error)
}
type Remote struct{ SSH bootstrap.Remote }

var diagnostic = regexp.MustCompile(`^[a-z0-9:_-]{1,100}$`)

func (r Remote) Call(ctx context.Context, q Request) (Observation, error) {
	if r.SSH == nil {
		return Observation{}, errors.New("authenticated transport required")
	}
	if _, ok := ctx.Deadline(); !ok {
		return Observation{}, errors.New("deadline required")
	}
	if err := q.Fleet.Validate(); err != nil {
		return Observation{}, err
	}
	if q.FleetID != q.Fleet.ID() {
		return Observation{}, errors.New("fleet ID mismatch")
	}
	found := false
	for _, t := range q.Fleet.Targets {
		if hash(t) == hash(q.Target) {
			found = true
		}
	}
	if !found {
		return Observation{}, errors.New("target outside fleet")
	}
	switch q.Action {
	case "observe", "join-profile":
	case "enroll":
		if q.Expected == nil {
			return Observation{}, errors.New("enrollment baseline required")
		}
	case "stage", "apply":
		if !digestRE.MatchString(q.OperationID) || q.Expected == nil || (q.Operation != "deploy" && q.Operation != "upgrade") {
			return Observation{}, errors.New("reviewed operation required")
		}
		for c, pin := range q.Pins {
			if q.Target.Containers[c] == "" || !validPin(pin) {
				return Observation{}, errors.New("unexpected component/image")
			}
		}
	default:
		return Observation{}, errors.New("unsupported managed action")
	}
	body, err := json.Marshal(q)
	if err != nil {
		return Observation{}, err
	}
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	_, _ = z.Write([]byte(worker))
	_ = z.Close()
	command := "/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/root /usr/bin/python3 -c \"import base64,zlib;exec(zlib.decompress(base64.b64decode('" + base64.StdEncoding.EncodeToString(compressed.Bytes()) + "')))\""
	if q.Fleet.Access.User != "root" {
		command = "sudo -n -- " + command
	}
	data, runErr := r.SSH.Run(ctx, transport.Endpoint{Address: q.Target.Address, Port: q.Fleet.Access.Port, HostAlias: fmt.Sprintf("%s.%s.%s.dashnet", q.Target.InstanceID, q.Fleet.Region, q.Fleet.AccountID)}, command, string(body))
	var o Observation
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	decodeErr := d.Decode(&o)
	var extra any
	if decodeErr == nil && !errors.Is(d.Decode(&extra), io.EOF) {
		decodeErr = errors.New("extra output")
	}
	if runErr != nil {
		if decodeErr == nil && diagnostic.MatchString(o.Error) {
			return Observation{}, fmt.Errorf("node %s: %w", o.Error, runErr)
		}
		return Observation{}, runErr
	}
	if decodeErr != nil || o.Error != "" || o.InstanceID != q.Target.InstanceID {
		return Observation{}, errors.New("invalid managed response; raw output withheld")
	}
	return o, nil
}
