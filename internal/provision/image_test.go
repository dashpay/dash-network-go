package provision

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func canonicalImage(i *types.Image) {
	i.BlockDeviceMappings = append(i.BlockDeviceMappings,
		types.BlockDeviceMapping{DeviceName: aws.String("/dev/sdb"), VirtualName: aws.String("ephemeral0")},
		types.BlockDeviceMapping{DeviceName: aws.String("/dev/sdc"), VirtualName: aws.String("ephemeral1")})
}

func TestCanonicalUbuntuImagePermitsNonBillableInstanceStoreDeclarations(t *testing.T) {
	p, cloud, store := setup(t)
	cloud.ImageHook = canonicalImage
	if _, err := run(context.Background(), p, cloud, store, "runner"); err != nil {
		t.Fatal(err)
	}
	for _, request := range cloud.Requests {
		if len(request.BlockDeviceMappings) != 1 || request.BlockDeviceMappings[0].Ebs == nil {
			t.Fatal("launch requested anything beyond the reviewed root disk")
		}
	}
}

func TestCanonicalImageCannotHideExtraOrAmbiguousDisks(t *testing.T) {
	for _, name := range []string{"extra-ebs", "mixed-ebs-ephemeral", "duplicate-device", "duplicate-virtual", "invalid-virtual", "missing-root", "small-root", "suppressed-root", "root-not-ebs"} {
		t.Run(name, func(t *testing.T) {
			p, cloud, store := setup(t)
			cloud.ImageHook = func(i *types.Image) {
				canonicalImage(i)
				switch name {
				case "extra-ebs":
					i.BlockDeviceMappings = append(i.BlockDeviceMappings, types.BlockDeviceMapping{DeviceName: aws.String("/dev/sdd"), Ebs: &types.EbsBlockDevice{VolumeSize: aws.Int32(100)}})
				case "mixed-ebs-ephemeral":
					i.BlockDeviceMappings[1].Ebs = &types.EbsBlockDevice{VolumeSize: aws.Int32(100)}
				case "duplicate-device":
					i.BlockDeviceMappings[1].DeviceName = i.RootDeviceName
				case "duplicate-virtual":
					i.BlockDeviceMappings[2].VirtualName = aws.String("ephemeral0")
				case "invalid-virtual":
					i.BlockDeviceMappings[1].VirtualName = aws.String("other")
				case "missing-root":
					i.BlockDeviceMappings = i.BlockDeviceMappings[1:]
				case "small-root":
					i.BlockDeviceMappings[0].Ebs.VolumeSize = aws.Int32(101)
				case "suppressed-root":
					i.BlockDeviceMappings[0].NoDevice = aws.String("")
				case "root-not-ebs":
					i.BlockDeviceMappings[0].Ebs = nil
				}
			}
			if _, err := run(context.Background(), p, cloud, store, "runner"); err == nil {
				t.Fatal("unsafe mapping accepted")
			}
			if len(cloud.Requests) != 0 || store.record.Kind != "" {
				t.Fatal("invalid mapping mutated AWS or the journal")
			}
		})
	}
}
