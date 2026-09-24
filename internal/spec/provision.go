package spec

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var resourceID = regexp.MustCompile(`^(vpc|subnet|sg|ami)-([0-9a-f]{8}|[0-9a-f]{17})$`)
var tableName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,255}$`)

func (n Network) ValidateProvision() error {
	p := n.AWS.Provision
	if p == nil {
		return errors.New("aws.provision is required for EC2 provisioning")
	}
	if n.Chain.Type == "testnet" {
		for _, g := range n.Nodes {
			if g.Role != "fullnode" {
				return errors.New("new testnet allocations currently support Core fullnodes only; use a separate allocation name and join plan")
			}
		}
	}
	if n.AWS.NetworkTagKey == "Name" || strings.HasPrefix(n.AWS.NetworkTagKey, "dashnet:") || strings.HasPrefix(strings.ToLower(n.AWS.NetworkTagKey), "aws:") || strings.ContainsAny(n.AWS.NetworkTagKey, "?\\") {
		return errors.New("provisioning network tag key conflicts with reserved tags or wildcard syntax")
	}
	if !tableName.MatchString(p.StateTable) {
		return errors.New("provision.stateTable must name an existing DynamoDB table")
	}
	validID := func(s, prefix string) bool { return strings.HasPrefix(s, prefix+"-") && resourceID.MatchString(s) }
	if !validID(p.VPCID, "vpc") || !validID(p.SubnetID, "subnet") {
		return errors.New("provision requires explicit VPC and subnet IDs")
	}
	if len(p.SecurityGroupIDs) == 0 || len(p.SecurityGroupIDs) > 5 {
		return errors.New("provision requires 1..5 explicit security groups")
	}
	seen := map[string]bool{}
	for _, id := range p.SecurityGroupIDs {
		if !validID(id, "sg") || seen[id] {
			return errors.New("security group IDs must be valid and unique")
		}
		seen[id] = true
	}
	if p.KeyName == "" || len(p.KeyName) > 255 || strings.ContainsAny(p.KeyName, "\n\r\t") {
		return errors.New("provision.keyName is required for emergency access")
	}
	if p.RootVolumeGiB < 8 || p.RootVolumeGiB > 16384 {
		return errors.New("rootVolumeGiB must be 8..16384")
	}
	if len(p.AMIs) != len(n.Architectures()) {
		return errors.New("provide exactly one AMI per requested architecture")
	}
	for _, arch := range n.Architectures() {
		ami := p.AMIs[arch]
		if !validID(ami.ID, "ami") || !account.MatchString(ami.OwnerID) {
			return fmt.Errorf("%s requires an exact AMI ID and owner account", arch)
		}
	}
	total := 0
	for _, g := range n.Nodes {
		total += g.Count
	}
	if total > 100 {
		return errors.New("initial EC2 executor is limited to 100 nodes per network")
	}
	return nil
}
