// Package provision implements only the devnet EC2 stage, not chain deployment.
package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/dashpay/dash-network-go/internal/spec"
)

const ManagedBy = "dash-network-go"

type Plan struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	ID         string       `json:"id"`
	Network    spec.Network `json:"network"`
	Targets    []Target     `json:"targets"`
}

type Target struct {
	Name         string `json:"name"`
	Group        string `json:"group"`
	Role         string `json:"role"`
	Architecture string `json:"architecture"`
	InstanceType string `json:"instanceType"`
	AMI          string `json:"ami"`
	RootDevice   string `json:"rootDevice"`
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func build(n spec.Network, roots map[string]string) Plan {
	p := Plan{APIVersion: spec.Version, Kind: "EC2ProvisionPlan", Network: n, Targets: []Target{}}
	for _, g := range n.Nodes {
		for i := 1; i <= g.Count; i++ {
			p.Targets = append(p.Targets, Target{Name: fmt.Sprintf("%s-%03d", g.Name, i), Group: g.Name, Role: g.Role, Architecture: g.Architecture, InstanceType: g.InstanceType, AMI: n.AWS.Provision.AMIs[g.Architecture].ID, RootDevice: roots[g.Architecture]})
		}
	}
	sort.Slice(p.Targets, func(i, j int) bool { return p.Targets[i].Name < p.Targets[j].Name })
	p.ID = digest(p)
	return p
}

func (p Plan) Validate() error {
	if err := p.Network.Validate(); err != nil {
		return err
	}
	if err := p.Network.ValidateProvision(); err != nil {
		return err
	}
	roots := map[string]string{}
	for _, t := range p.Targets {
		if !strings.HasPrefix(t.RootDevice, "/dev/") || len(t.RootDevice) > 64 || strings.ContainsAny(t.RootDevice, " \n\r\t") {
			return errors.New("invalid root device in provision plan")
		}
		if old, ok := roots[t.Architecture]; ok && old != t.RootDevice {
			return errors.New("inconsistent root devices")
		}
		roots[t.Architecture] = t.RootDevice
	}
	if !reflect.DeepEqual(p, build(p.Network, roots)) {
		return errors.New("provision plan is altered, incomplete, or has an invalid ID; regenerate it")
	}
	return nil
}

// Key excludes generation: a changed generation cannot bypass the network lock
// or silently replace a previous fleet. This first stage never rotates records.
func (p Plan) Key() string {
	return p.Network.AWS.AccountID + "/" + p.Network.AWS.Region + "/" + p.Network.Metadata.Name
}
func (p Plan) Token(t Target) string { return digest([]string{p.ID, t.Name}) }
func (p Plan) Tags(t Target) map[string]string {
	return map[string]string{
		"Name":                      p.Network.Metadata.Name + "-" + t.Name,
		p.Network.AWS.NetworkTagKey: p.Network.Metadata.Name,
		"dashnet:managed-by":        ManagedBy,
		"dashnet:network":           p.Network.Metadata.Name,
		"dashnet:generation":        fmt.Sprint(p.Network.Chain.Generation),
		"dashnet:plan":              p.ID,
		"dashnet:node":              t.Name,
		"dashnet:role":              t.Role,
	}
}
