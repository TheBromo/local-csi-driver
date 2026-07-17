// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"testing"
)

// sampleLsblk is representative lsblk --json output for a resource disk
// with one mounted partition. Newer lsblk reports both mountpoint and
// mountpoints.
const sampleLsblk = `{
  "blockdevices": [
    {
      "path": "/dev/sdb", "kname": "sdb", "type": "disk", "fstype": null,
      "maj:min": "8:16", "mountpoint": null, "mountpoints": [null],
      "children": [
        {
          "path": "/dev/sdb1", "kname": "sdb1", "type": "part", "fstype": "ext4",
          "maj:min": "8:17", "mountpoint": "/mnt", "mountpoints": ["/mnt"]
        }
      ]
    }
  ]
}`

func TestParseLsblkTree(t *testing.T) {
	t.Parallel()
	disk, err := parseLsblkTree([]byte(sampleLsblk))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disk.Path != "/dev/sdb" || disk.Type != diskType || disk.MajMin != "8:16" {
		t.Errorf("unexpected disk: %+v", disk)
	}
	if len(disk.Children) != 1 || disk.Children[0].FSType != "ext4" {
		t.Errorf("unexpected children: %+v", disk.Children)
	}

	if _, err := parseLsblkTree([]byte(`{"blockdevices": []}`)); err == nil {
		t.Error("expected error for empty device list")
	}
	if _, err := parseLsblkTree([]byte(`not json`)); err == nil {
		t.Error("expected error for invalid json")
	}
}

func TestDeviceMounts(t *testing.T) {
	t.Parallel()
	disk, err := parseLsblkTree([]byte(sampleLsblk))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mounts := disk.mounts()
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount (deduplicated), got %+v", mounts)
	}
	if mounts[0].Device != "/dev/sdb1" || mounts[0].Target != mntTarget {
		t.Errorf("unexpected mount: %+v", mounts[0])
	}
}

func TestDevicePartitionsAndHolders(t *testing.T) {
	t.Parallel()
	disk := &Device{
		Path: "/dev/sdb", Type: diskType,
		Children: []Device{
			{Path: "/dev/sdb1", Type: partType},
			{Path: "/dev/sdb2", Type: partType, Children: []Device{
				{Path: "/dev/mapper/crypt0", Type: "crypt"},
			}},
			{Path: "/dev/md0", Type: "raid0"},
		},
	}
	parts := disk.partitions()
	if len(parts) != 2 {
		t.Errorf("expected 2 partitions, got %+v", parts)
	}
	holders := disk.foreignHolders()
	if len(holders) != 2 {
		t.Errorf("expected 2 foreign holders (crypt + raid), got %+v", holders)
	}
}
