package node

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"slices"
)

// The optional control recipe is separate from the historical node/upgrade
// recipes: old deployment artifacts retain their exact recipe identities.
//
//go:embed mining.py
var miningRecipe string

func MiningDigest() string {
	s := sha256.Sum256([]byte(recipe + observer + miningRecipe))
	return hex.EncodeToString(s[:])
}

type MiningControl struct {
	OperationID  string `json:"operationId"`
	ContainerID  string `json:"containerId"`
	RecipeSHA256 string `json:"recipeSha256"`
}
type MiningPause struct {
	OperationID string `json:"operationId"`
	ContainerID string `json:"containerId"`
	Paused      bool   `json:"paused"`
	ExpiresAt   int64  `json:"expiresAt"`
}

func miningAction(action string) bool {
	return slices.Contains([]string{"rollout-mining-pause", "rollout-mining-status", "rollout-mining-renew", "rollout-mining-resume"}, action)
}
func (q Request) validateMining() error {
	m := q.MiningControl
	if m == nil || !changeID.MatchString(m.OperationID) || !changeID.MatchString(m.ContainerID) || m.RecipeSHA256 != MiningDigest() || q.Target.Name != q.Context.MiningNodeName || (q.Target.Role != "miner" && q.Target.Role != "wallet") {
		return errors.New("plan-bound owned miner control required")
	}
	return nil
}
