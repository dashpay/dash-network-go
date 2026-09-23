package bootstrap

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/dashpay/dash-network-go/internal/transport"
)

//go:embed recipe.sh
var recipe string

type Remote interface {
	Run(context.Context, transport.Endpoint, string, string) ([]byte, error)
}
type Observation struct {
	PlanID         string `json:"planId"`
	InstanceID     string `json:"instanceId"`
	Architecture   string `json:"architecture"`
	Ready          bool   `json:"ready"`
	DockerVersion  string `json:"dockerVersion"`
	ComposeVersion string `json:"composeVersion"`
	Error          string `json:"error,omitempty"`
}

var versionPattern = regexp.MustCompile(`^[a-zA-Z0-9.+:~_-]{1,100}$`)
var instancePattern = regexp.MustCompile(`^i-([0-9a-f]{8}|[0-9a-f]{17})$`)

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// Command only renders a validated recipe and typed arguments, never user shell.
func Command(p Plan, t Target, instanceID, mode string) (string, string, error) {
	if err := p.Validate(); err != nil {
		return "", "", err
	}
	if mode != "probe" && mode != "apply" {
		return "", "", errors.New("invalid bootstrap mode")
	}
	if !instancePattern.MatchString(instanceID) {
		return "", "", errors.New("invalid instance identity")
	}
	matched := false
	for _, expected := range p.Targets {
		if expected.Name == t.Name {
			a, _ := json.Marshal(t)
			b, _ := json.Marshal(expected)
			matched = bytes.Equal(a, b)
		}
	}
	if !matched {
		return "", "", errors.New("target does not match bootstrap plan")
	}
	args := []string{mode, p.Compute.ID + ":" + instanceID, p.ID, instanceID, t.Architecture}
	for _, image := range t.Images {
		args = append(args, image.Pinned)
	}
	command := "/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/root /bin/bash -s --"
	if p.Access.User != "root" {
		command = "sudo -n -- " + command
	}
	for _, arg := range args {
		command += " " + quote(arg)
	}
	return command, recipe, nil
}

func Observe(ctx context.Context, remote Remote, endpoint transport.Endpoint, p Plan, t Target, id, mode string) (Observation, error) {
	command, script, err := Command(p, t, id, mode)
	if err != nil {
		return Observation{}, err
	}
	data, runErr := remote.Run(ctx, endpoint, command, script)
	var o Observation
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	decodeErr := d.Decode(&o)
	var extra any
	if decodeErr == nil && !errors.Is(d.Decode(&extra), io.EOF) {
		decodeErr = errors.New("trailing remote output")
	}
	// Only fixed recipe stage names may enter durable error messages.
	stages := map[string]bool{"prerequisites": true, "os-check": true, "instance-identity": true, "cloud-init": true, "host-lock": true, "host-ownership": true, "existing-containers": true, "runtime-install": true, "runtime-start": true, "image-pull": true, "runtime-verify": true, "checkpoint": true, "response": true}
	if runErr != nil {
		if decodeErr == nil && stages[o.Error] {
			return Observation{}, fmt.Errorf("bootstrap stage %s: %w", o.Error, runErr)
		}
		return Observation{}, runErr
	}
	if decodeErr != nil || o.Error != "" || o.PlanID != p.ID || o.InstanceID != id || o.Architecture != t.Architecture {
		return Observation{}, errors.New("invalid or mismatched bootstrap response; raw output withheld")
	}
	if o.Ready && (!versionPattern.MatchString(o.DockerVersion) || !versionPattern.MatchString(o.ComposeVersion)) {
		return Observation{}, errors.New("ready response lacks valid runtime versions")
	}
	return o, nil
}
