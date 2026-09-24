package node

import (
	"strings"
	"testing"
)

func TestJoinRejectsPrivateOrArbitraryConfiguration(t *testing.T) {
	j := CoreJoin{ChainType: "devnet", CoreNetwork: "devnet-existing", Genesis: strings.Repeat("a", 64), CheckpointHeight: 100, CheckpointHash: strings.Repeat("b", 64), Peers: []string{"10.0.0.1:20001"}}
	if e := j.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, v := range []string{"sporkkey=private", "masternodeblsprivkey=secret", "rpcpassword=secret", "includeconf=/etc/passwd", "datadir=/var/lib/other", "llmqplatform=foo\nserver=0", "powtargetspacing=-1"} {
		j.Options = []string{v}
		if j.Validate() == nil {
			t.Fatal("accepted", v)
		}
	}
	j.Options = []string{"powtargetspacing=10", "minimumdifficultyblocks=1000000", "llmqplatform=llmq_devnet_platform"}
	if e := j.Validate(); e != nil {
		t.Fatal(e)
	}
	j.ChainType = "testnet"
	j.CoreNetwork = "test"
	if j.Validate() == nil {
		t.Fatal("testnet consensus override")
	}
}
