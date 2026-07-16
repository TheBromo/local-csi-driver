// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"strings"
	"testing"
)

func TestParseDeviceGraphCapturesChildrenAndHolders(t *testing.T) {
	t.Parallel()

	children := []byte(`{
  "blockdevices": [{
    "name": "/dev/sdb", "path": "/dev/sdb", "kname": "/dev/sdb",
    "maj:min": "8:16", "type": "disk", "fstype": null, "pttype": "dos",
    "mountpoints": [null],
    "children": [{
      "name": "/dev/sdb1", "path": "/dev/sdb1", "kname": "/dev/sdb1",
      "pkname": "/dev/sdb", "maj:min": "8:17", "type": "part",
      "fstype": "ext4", "pttype": null, "mountpoints": ["/mnt"]
    }]
  }]
}`)
	holders := []byte(`{
  "blockdevices": [{
    "name": "/dev/sdb", "path": "/dev/sdb", "maj:min": "8:16", "type": "disk",
    "mountpoints": [null],
    "children": [{"name": "/dev/mapper/foreign", "path": "/dev/mapper/foreign", "maj:min": "253:0", "type": "crypt", "mountpoints": [null]}]
  }]
}`)

	graph, err := parseDeviceGraph(children, holders, "8:16")
	if err != nil {
		t.Fatalf("parseDeviceGraph() error = %v", err)
	}
	if graph.Disk.Type != deviceTypeDisk || graph.Disk.Path != fixtureResourceDisk {
		t.Fatalf("disk = %#v, want /dev/sdb disk", graph.Disk)
	}
	if _, ok := graph.Candidates["8:17"]; !ok {
		t.Fatal("candidate graph does not contain resource partition identity 8:17")
	}
	if len(graph.Holders) != 1 || graph.Holders[0].Type != deviceTypeCrypt {
		t.Fatalf("holders = %#v, want one crypt holder", graph.Holders)
	}
	if err := graph.rejectForeignHolders(false); err == nil || !strings.Contains(err.Error(), "crypt holder") {
		t.Fatalf("rejectForeignHolders() error = %v, want crypt holder rejection", err)
	}
}

func TestValidateCandidateMounts(t *testing.T) {
	t.Parallel()

	partition := BlockDevice{Path: fixtureResourcePartition, MajorMinor: "8:17", Type: deviceTypePart, Filesystem: "ext4"}
	graph := &DeviceGraph{
		Disk: BlockDevice{
			Path:       fixtureResourceDisk,
			MajorMinor: "8:16",
			Type:       deviceTypeDisk,
			Children:   []BlockDevice{partition},
		},
		Candidates: map[string]BlockDevice{"8:16": {}, "8:17": partition},
	}

	tests := []struct {
		name      string
		mounts    []Mount
		wantMatch bool
		wantError string
	}{
		{name: "unmounted", wantMatch: false},
		{
			name:      "recognized resource partition",
			mounts:    []Mount{{Source: fixtureResourcePartition, Target: "/mnt", MajorMinor: "8:17"}},
			wantMatch: true,
		},
		{
			name:      "protected runtime mount",
			mounts:    []Mount{{Source: fixtureResourcePartition, Target: "/var/lib/kubelet", MajorMinor: "8:17"}},
			wantError: "not the recognized resource partition",
		},
		{
			name: "holder backs root",
			mounts: []Mount{{
				Source:     "/dev/mapper/os-root",
				Target:     "/",
				MajorMinor: "253:0",
			}},
			wantError: "not the recognized resource partition",
		},
		{
			name: "pod mount",
			mounts: []Mount{
				{Source: fixtureResourcePartition, Target: "/mnt", MajorMinor: "8:17"},
				{Source: fixtureResourcePartition, Target: "/var/lib/kubelet/pods/a/volumes/b", MajorMinor: "8:17"},
			},
			wantError: "2 backed mounts",
		},
	}
	graph.Holders = []BlockDevice{{Path: "/dev/mapper/os-root", MajorMinor: "253:0", Type: deviceTypeLVM}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			match, err := validateCandidateMounts(graph, test.mounts)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("validateCandidateMounts() error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateCandidateMounts() error = %v", err)
			}
			if (match != nil) != test.wantMatch {
				t.Fatalf("validateCandidateMounts() match = %#v, wantMatch %t", match, test.wantMatch)
			}
		})
	}
}

func TestParseLVMReports(t *testing.T) {
	t.Parallel()

	pvs, err := parsePhysicalVolumes([]byte(`{"report":[{"pv":[{"pv_name":"/dev/sdb","vg_name":"containerstorage"}]}]}`))
	if err != nil {
		t.Fatalf("parsePhysicalVolumes() error = %v", err)
	}
	if len(pvs) != 1 || pvs[0].VolumeGroup != "containerstorage" {
		t.Fatalf("physical volumes = %#v", pvs)
	}

	vgs, err := parseVolumeGroups([]byte(`{"report":[{"vg":[{"vg_name":"containerstorage","vg_tags":"other,local-csi"}]}]}`))
	if err != nil {
		t.Fatalf("parseVolumeGroups() error = %v", err)
	}
	if len(vgs) != 1 || !hasTag(vgs[0].Tags, "local-csi") {
		t.Fatalf("volume groups = %#v, want local-csi tag", vgs)
	}
}
