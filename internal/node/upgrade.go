package node

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"reflect"
	"regexp"

	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/google/go-containerregistry/pkg/name"
)

//go:embed upgrade.py
var upgradeRecipe string

func UpgradeDigest() string {
	s := sha256.Sum256([]byte(recipe + observer + upgradeRecipe))
	return hex.EncodeToString(s[:])
}

type ImageChange struct {
	ID         string                 `json:"id"`
	PreviousID string                 `json:"previousId,omitempty"`
	From       provision.ImageSet     `json:"from"`
	To         provision.ImageSet     `json:"to"`
	Preserve   provision.Preservation `json:"preserve"`
}

var changeID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (q Request) validateUpgrade() error {
	if q.Upgrade == nil || !changeID.MatchString(q.Upgrade.ID) || (q.Upgrade.PreviousID != "" && !changeID.MatchString(q.Upgrade.PreviousID)) || q.Target.Role != "validator" {
		return errors.New("upgrade requires exact validator and reviewed change identity")
	}
	u := q.Upgrade
	if len(u.From) != 6 || len(u.To) != 6 || u.From["core"] != u.To["core"] {
		return errors.New("upgrade requires complete image sets and unchanged Core")
	}
	for _, component := range spec.Components {
		for _, pins := range []provision.ImageSet{u.From, u.To} {
			ref, err := name.NewDigest(pins[component], name.StrictValidation)
			if err != nil || len(ref.DigestStr()) != 71 || ref.DigestStr()[:7] != "sha256:" || !changeID.MatchString(ref.DigestStr()[7:]) {
				return errors.New("upgrade images must be SHA256 references for every component")
			}
		}
	}
	current := provision.ImageSet{}
	for _, image := range q.Target.Images {
		current[image.Component] = image.Pinned
	}
	if !reflect.DeepEqual(current, u.From) && !reflect.DeepEqual(current, u.To) {
		return errors.New("upgrade target is outside reviewed image sets")
	}
	if !changeID.MatchString(u.Preserve.CoreID) || !changeID.MatchString(u.Preserve.CoreConfig) || !changeID.MatchString(u.Preserve.CoreGenesis) || u.Preserve.CoreStarted == "" {
		return errors.New("upgrade lacks Core preservation evidence")
	}
	return nil
}
