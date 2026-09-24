package node

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// CoreJoin is a public chain contract, not a copied wallet or validator profile.
// Genesis is block 1 for devnet, block 0 for testnet. Checkpoint is an independently
// reviewed confirmed block from the existing chain, never a new genesis.
type CoreJoin struct {
	ChainType        string   `json:"chainType"`
	CoreNetwork      string   `json:"coreNetwork"`
	Genesis          string   `json:"genesis"`
	CheckpointHeight int64    `json:"checkpointHeight"`
	CheckpointHash   string   `json:"checkpointHash"`
	Peers            []string `json:"peers"`
	Options          []string `json:"options"`
}

var joinHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var joinNetwork = regexp.MustCompile(`^devnet-[a-z][a-z0-9-]{0,49}$`)
var publicAddress = regexp.MustCompile(`^[123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz]{26,40}$`)
var quorumOption = regexp.MustCompile(`^llmq_[a-z0-9_]{1,40}$`)

func (j CoreJoin) Validate() error {
	if (j.ChainType != "devnet" && j.ChainType != "testnet") || (j.ChainType == "testnet" && j.CoreNetwork != "test") || (j.ChainType == "devnet" && !joinNetwork.MatchString(j.CoreNetwork)) {
		return errors.New("explicit existing Core chain identity required")
	}
	if !joinHash.MatchString(j.Genesis) || !joinHash.MatchString(j.CheckpointHash) || j.CheckpointHeight < 2 || len(j.Peers) < 1 || len(j.Peers) > 32 {
		return errors.New("confirmed genesis/checkpoint and 1..32 explicit peers required")
	}
	seen := map[string]bool{}
	for _, p := range j.Peers {
		host, port, e := net.SplitHostPort(p)
		ip := net.ParseIP(host)
		n, e2 := strconv.Atoi(port)
		if e != nil || e2 != nil || ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() || n < 1 || n > 65535 || seen[p] {
			return errors.New("join peers require unique usable IPv4:port endpoints")
		}
		seen[p] = true
	}
	if len(j.Options) > 32 || (j.ChainType == "testnet" && len(j.Options) > 0) {
		return errors.New("testnet consensus uses compiled defaults; devnet public options limited to 32")
	}
	seen = map[string]bool{}
	for _, option := range j.Options {
		k, v, ok := strings.Cut(option, "=")
		if !ok || strings.ContainsAny(option, "\r\n\t []#;") || seen[k] {
			return errors.New("invalid or duplicate join option")
		}
		seen[k] = true
		switch k {
		case "sporkaddr":
			if !publicAddress.MatchString(v) {
				return errors.New("sporkaddr requires public address")
			}
		case "llmqchainlocks", "llmqinstantsend", "llmqinstantsenddip0024", "llmqplatform", "llmqmnhf":
			if !quorumOption.MatchString(v) {
				return errors.New("invalid quorum name")
			}
		case "minimumdifficultyblocks", "highsubsidyblocks", "highsubsidyfactor", "powtargetspacing":
			n, e := strconv.ParseUint(v, 10, 32)
			if e != nil || n == 0 {
				return errors.New("positive bounded devnet chain option required")
			}
		default:
			return errors.New("unsupported join option; private identities, arbitrary config and RPC options are forbidden")
		}
	}
	return nil
}

//go:embed join.py
var joinRecipe string

func JoinRecipeDigest() string {
	s := sha256.Sum256([]byte(recipe + joinRecipe))
	return hex.EncodeToString(s[:])
}
func (q Request) validateJoin() error {
	if q.Join == nil || q.Target.Role != "fullnode" || q.Context.CoreNetwork != q.Join.CoreNetwork || !joinHash.MatchString(q.Context.PlanID) || !joinHash.MatchString(q.Context.ComputePlanID) || !joinHash.MatchString(q.Context.BootstrapID) {
		return errors.New("join requires exact owned fullnode plan")
	}
	return q.Join.Validate()
}
