// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testVG        = "containerstorage"
	resourceLink  = DefaultResourceDiskLink
	cloudPartLink = "/dev/disk/cloud/azure_resource-part1"
	cloudInitDone = "status: done\n"
)

// cloudInitFstab is the fstab layout cloud-init produces on AKS Ubuntu.
const cloudInitFstab = `# CLOUD_IMG: This file was created/modified by the Cloud Image build process
UUID=1111 / ext4 discard,errors=remount-ro 0 1
UUID=2222 /boot/efi vfat umask=0077 0 1
/dev/disk/cloud/azure_resource-part1	/mnt	auto	defaults,nofail,x-systemd.after=cloud-init.service,_netdev,comment=cloudconfig	0	2
`

// testConfig returns a Config against a temp host /etc seeded with the
// cloud-init fstab. Tests overwrite it with writeFstab where needed.
func testConfig(t *testing.T) Config {
	t.Helper()
	etc := t.TempDir()
	if err := os.WriteFile(filepath.Join(etc, "fstab"), []byte(cloudInitFstab), 0644); err != nil {
		t.Fatalf("failed to write fstab: %v", err)
	}
	return Config{
		VolumeGroup:                 testVG,
		Required:                    true,
		AllowDestructivePreparation: true,
		CloudInitTimeout:            5 * time.Second,
		HostEtcDir:                  etc,
		LockFile:                    filepath.Join(t.TempDir(), "lock"),
	}
}

// resourceDiskTree returns the lsblk tree of a resource disk holding the
// cloud-init layout: a single ext4 partition, mounted at target (may be
// empty for unmounted).
func resourceDiskTree(disk, majMin, target string) *Device {
	part := Device{
		Path:   disk + "1",
		KName:  disk + "1",
		Type:   partType,
		FSType: resourceFilesystem,
		MajMin: "8:17",
	}
	if target != "" {
		part.Mountpoints = []string{target}
	}
	return &Device{
		Path:     disk,
		KName:    disk,
		Type:     diskType,
		MajMin:   majMin,
		Children: []Device{part},
	}
}

// blankDiskTree returns the lsblk tree of /dev/sdb without partitions or
// signatures.
func blankDiskTree() *Device {
	return &Device{Path: "/dev/sdb", KName: "/dev/sdb", Type: diskType, MajMin: "8:16"}
}

// cloudInitScenario wires a fake host in the state a fresh AKS Ubuntu node
// is in: resource disk sdb with a single ext4 partition mounted at /mnt and
// the cloud-init fstab entry.
func cloudInitScenario(t *testing.T, disk string) (*fakeHost, Config) {
	t.Helper()
	host := newFakeHost(t)
	host.linkTargets[resourceLink] = []string{disk}
	host.linkTargets[cloudPartLink] = []string{disk + "1"}
	host.trees[disk] = resourceDiskTree(disk, "8:16", mntTarget)
	host.otherMounts = []string{"/", "/boot/efi", "/var/lib/kubelet"}
	host.cloudInitOutput = cloudInitDone
	return host, testConfig(t)
}

func prepare(t *testing.T, host *fakeHost, cfg Config) error {
	t.Helper()
	p, err := New(cfg, host)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	return p.Prepare(context.Background())
}

func TestPrepareCloudInitLayout(t *testing.T) {
	t.Parallel()
	host, cfg := cloudInitScenario(t, "/dev/sdb")

	if err := prepare(t, host, cfg); err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}

	// The volume group must exist, be tagged, and contain the disk.
	if vg, ok := host.vgs[testVG]; !ok || vg.tags != DefaultVolumeGroupTag {
		t.Errorf("volume group not created with tag: %+v", host.vgs)
	}
	if vg := host.pvs["/dev/sdb"]; vg != testVG {
		t.Errorf("expected /dev/sdb to be a PV of %s, got %q", testVG, vg)
	}

	// The fstab entry must be commented out and a backup taken.
	content, err := os.ReadFile(filepath.Join(cfg.HostEtcDir, "fstab"))
	if err != nil {
		t.Fatalf("failed to read fstab: %v", err)
	}
	if !strings.Contains(string(content), fstabNeutralizedPrefix+"/dev/disk/cloud/azure_resource-part1") {
		t.Errorf("fstab /mnt entry was not neutralized:\n%s", content)
	}
	if strings.Count(string(content), "UUID=1111") != 1 || strings.Contains(string(content), fstabNeutralizedPrefix+"UUID") {
		t.Errorf("unrelated fstab entries were modified:\n%s", content)
	}
	if _, err := os.Stat(filepath.Join(cfg.HostEtcDir, "fstab"+fstabBackupSuffix)); err != nil {
		t.Errorf("fstab backup missing: %v", err)
	}

	// The partition must be wiped before the whole disk.
	partWipes := host.callsMatching("wipefs --all /dev/sdb1")
	diskWipes := host.callsMatching("wipefs --all --force /dev/sdb")
	if len(partWipes) != 1 || len(diskWipes) != 1 {
		t.Errorf("expected one partition and one disk wipe, got %v / %v", partWipes, diskWipes)
	}
}

func TestPrepareLinkTargetVariants(t *testing.T) {
	t.Parallel()
	for _, disk := range []string{"/dev/sdb", "/dev/sdz", "/dev/sdaa"} {
		t.Run(disk, func(t *testing.T) {
			t.Parallel()
			host, cfg := cloudInitScenario(t, disk)
			if err := prepare(t, host, cfg); err != nil {
				t.Fatalf("Prepare() failed for %s: %v", disk, err)
			}
			if vg := host.pvs[disk]; vg != testVG {
				t.Errorf("expected %s to be a PV of %s, got %q", disk, testVG, vg)
			}
		})
	}
}

func TestPrepareIdempotentSecondRun(t *testing.T) {
	t.Parallel()
	host, cfg := cloudInitScenario(t, "/dev/sdb")

	if err := prepare(t, host, cfg); err != nil {
		t.Fatalf("first Prepare() failed: %v", err)
	}
	wipesAfterFirst := len(host.callsMatching("wipefs"))
	fstabAfterFirst, err := os.ReadFile(filepath.Join(cfg.HostEtcDir, "fstab"))
	if err != nil {
		t.Fatalf("failed to read fstab: %v", err)
	}

	if err := prepare(t, host, cfg); err != nil {
		t.Fatalf("second Prepare() failed: %v", err)
	}
	if got := len(host.callsMatching("wipefs")); got != wipesAfterFirst {
		t.Errorf("second run wiped the disk again: %d wipefs calls, expected %d", got, wipesAfterFirst)
	}
	fstabAfterSecond, err := os.ReadFile(filepath.Join(cfg.HostEtcDir, "fstab"))
	if err != nil {
		t.Fatalf("failed to read fstab: %v", err)
	}
	if string(fstabAfterFirst) != string(fstabAfterSecond) {
		t.Errorf("second run modified fstab again")
	}
}

func TestPrepareManagedVGNoWipe(t *testing.T) {
	t.Parallel()
	host := newFakeHost(t)
	host.linkTargets[resourceLink] = []string{"/dev/sdb"}
	host.trees["/dev/sdb"] = blankDiskTree()
	host.trees["/dev/sdb"].FSType = "LVM2_member"
	host.pvs["/dev/sdb"] = testVG
	host.vgs[testVG] = &vgInfo{tags: DefaultVolumeGroupTag}
	host.cloudInitOutput = cloudInitDone
	cfg := testConfig(t)

	if err := prepare(t, host, cfg); err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}
	if calls := host.callsMatching("wipefs"); len(calls) > 0 {
		t.Errorf("managed VG must never be wiped, got: %v", calls)
	}
	if calls := host.callsMatching("vgcreate"); len(calls) > 0 {
		t.Errorf("managed VG must not be recreated, got: %v", calls)
	}
	if calls := host.callsMatching("vgchange --activate y " + testVG); len(calls) != 1 {
		t.Errorf("expected the managed VG to be activated, got: %v", calls)
	}
}

func TestPrepareManagedVGMissingTag(t *testing.T) {
	t.Parallel()
	host := newFakeHost(t)
	host.linkTargets[resourceLink] = []string{"/dev/sdb"}
	host.trees["/dev/sdb"] = blankDiskTree()
	host.pvs["/dev/sdb"] = testVG
	host.vgs[testVG] = &vgInfo{tags: "someone-else"}
	host.cloudInitOutput = cloudInitDone
	cfg := testConfig(t)

	err := prepare(t, host, cfg)
	if err == nil || !strings.Contains(err.Error(), "ownership tag") {
		t.Fatalf("expected ownership tag error, got: %v", err)
	}
}

func TestPrepareInterruptedPVRecovery(t *testing.T) {
	t.Parallel()
	host := newFakeHost(t)
	host.linkTargets[resourceLink] = []string{"/dev/sdb"}
	host.trees["/dev/sdb"] = blankDiskTree()
	host.trees["/dev/sdb"].FSType = "LVM2_member"
	host.pvs["/dev/sdb"] = "" // PV without a VG: interrupted run.
	host.cloudInitOutput = cloudInitDone
	cfg := testConfig(t)

	if err := prepare(t, host, cfg); err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}
	if calls := host.callsMatching("wipefs"); len(calls) > 0 {
		t.Errorf("interrupted PV recovery must not wipe, got: %v", calls)
	}
	if vg, ok := host.vgs[testVG]; !ok || vg.tags != DefaultVolumeGroupTag {
		t.Errorf("volume group not created with tag: %+v", host.vgs)
	}
}

func TestPrepareBlankDisk(t *testing.T) {
	t.Parallel()
	host := newFakeHost(t)
	host.linkTargets[resourceLink] = []string{"/dev/sdb"}
	host.trees["/dev/sdb"] = blankDiskTree()
	host.cloudInitOutput = cloudInitDone
	cfg := testConfig(t)

	if err := prepare(t, host, cfg); err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}
	if calls := host.callsMatching("wipefs"); len(calls) > 0 {
		t.Errorf("blank disk must not be wiped, got: %v", calls)
	}
	if vg := host.pvs["/dev/sdb"]; vg != testVG {
		t.Errorf("expected /dev/sdb to be a PV of %s, got %q", testVG, vg)
	}
}

func TestPrepareForeignVGOnResourceDisk(t *testing.T) {
	t.Parallel()
	host := newFakeHost(t)
	host.linkTargets[resourceLink] = []string{"/dev/sdb"}
	host.trees["/dev/sdb"] = blankDiskTree()
	host.pvs["/dev/sdb"] = "foreignvg"
	host.cloudInitOutput = cloudInitDone
	cfg := testConfig(t)

	err := prepare(t, host, cfg)
	if err == nil || !strings.Contains(err.Error(), "foreign volume group") {
		t.Fatalf("expected foreign volume group error, got: %v", err)
	}
	if calls := host.callsMatching("wipefs"); len(calls) > 0 {
		t.Errorf("foreign VG must never be wiped, got: %v", calls)
	}
}

func TestPrepareExpectedVGOnAnotherDisk(t *testing.T) {
	t.Parallel()
	host := newFakeHost(t)
	host.linkTargets[resourceLink] = []string{"/dev/sdb"}
	host.trees["/dev/sdb"] = blankDiskTree()
	host.pvs["/dev/sdc"] = testVG
	host.vgs[testVG] = &vgInfo{tags: DefaultVolumeGroupTag}
	host.cloudInitOutput = cloudInitDone
	cfg := testConfig(t)

	err := prepare(t, host, cfg)
	if err == nil || !strings.Contains(err.Error(), "already exists on other devices") {
		t.Fatalf("expected VG-on-other-disk error, got: %v", err)
	}
}

func TestPrepareLinkMissing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		required  bool
		expectErr bool
	}{
		{name: "required fails", required: true, expectErr: true},
		{name: "optional no-op", required: false, expectErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := newFakeHost(t)
			host.linkTargets[resourceLink] = []string{""}
			host.cloudInitOutput = cloudInitDone
			cfg := testConfig(t)
			cfg.Required = tc.required

			err := prepare(t, host, cfg)
			if tc.expectErr && (err == nil || !strings.Contains(err.Error(), "could not be resolved")) {
				t.Fatalf("expected missing-link error, got: %v", err)
			}
			if !tc.expectErr {
				if err != nil {
					t.Fatalf("expected no-op success, got: %v", err)
				}
				for _, forbidden := range []string{"wipefs", "pvcreate", "vgcreate", "umount"} {
					if calls := host.callsMatching(forbidden); len(calls) > 0 {
						t.Errorf("no-op run executed %v", calls)
					}
				}
			}
		})
	}
}

func TestPrepareLinkResolvesToPartition(t *testing.T) {
	t.Parallel()
	host := newFakeHost(t)
	host.linkTargets[resourceLink] = []string{"/dev/sdb1"}
	host.trees["/dev/sdb1"] = &Device{Path: "/dev/sdb1", Type: partType, MajMin: "8:17"}
	host.cloudInitOutput = cloudInitDone
	cfg := testConfig(t)

	err := prepare(t, host, cfg)
	if err == nil || !strings.Contains(err.Error(), "expected a whole disk") {
		t.Fatalf("expected whole-disk error, got: %v", err)
	}
}

func TestPrepareIdentityChangedBeforeWipe(t *testing.T) {
	t.Parallel()
	host, cfg := cloudInitScenario(t, "/dev/sdb")
	// First resolution (preflight) returns sdb, the re-check before the
	// wipe resolves to a different disk.
	host.linkTargets[resourceLink] = []string{"/dev/sdb", "/dev/sdc"}
	host.trees["/dev/sdc"] = resourceDiskTree("/dev/sdc", "8:32", "")

	err := prepare(t, host, cfg)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("expected identity change error, got: %v", err)
	}
	if calls := host.callsMatching("wipefs"); len(calls) > 0 {
		t.Errorf("no wipe may happen after an identity change, got: %v", calls)
	}
}

func TestPrepareCloudInit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		hang      bool
		output    string
		err       error
		expectErr string
	}{
		{name: "timeout", hang: true, expectErr: "timed out"},
		{name: "error", output: "status: error\n", err: errors.New("exit status 1"), expectErr: "did not finish successfully"},
		{name: "degraded tolerated", output: "status: degraded\n", err: errors.New("exit status 2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host, cfg := cloudInitScenario(t, "/dev/sdb")
			host.cloudInitHang = tc.hang
			host.cloudInitOutput = tc.output
			host.cloudInitErr = tc.err
			if tc.hang {
				cfg.CloudInitTimeout = 50 * time.Millisecond
			}

			err := prepare(t, host, cfg)
			if tc.expectErr == "" {
				if err != nil {
					t.Fatalf("expected success, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.expectErr) {
				t.Fatalf("expected %q error, got: %v", tc.expectErr, err)
			}
		})
	}
}

func TestPrepareRejectsUnrecognizedStates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		mutate    func(t *testing.T, host *fakeHost, cfg *Config)
		expectErr string
	}{
		{
			name: "custom mnt entry",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				writeFstab(t, cfg.HostEtcDir, "/dev/disk/cloud/azure_resource-part1 /mnt ext4 defaults 0 2\n")
			},
			expectErr: "not cloud-init managed",
		},
		{
			name: "ambiguous mnt entries",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				writeFstab(t, cfg.HostEtcDir, cloudInitFstab+"/dev/sdc1 /mnt ext4 defaults,comment=cloudconfig 0 2\n")
			},
			expectErr: "ambiguous /mnt configuration",
		},
		{
			name: "mnt entry resolves to another device",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.linkTargets[cloudPartLink] = []string{"/dev/sdc1"}
				host.trees["/dev/sdc1"] = &Device{Path: "/dev/sdc1", Type: partType}
			},
			expectErr: "not the resource partition",
		},
		{
			name: "mnt entry with non-device source",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				writeFstab(t, cfg.HostEtcDir, "UUID=3333 /mnt ext4 defaults,comment=cloudconfig 0 2\n")
			},
			expectErr: "not a recognized cloud-init device path",
		},
		{
			name: "ext4 partition without fstab entry",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				writeFstab(t, cfg.HostEtcDir, "UUID=1111 / ext4 defaults 0 1\n")
				host.unmountMnt()
			},
			expectErr: "not treated as disposable",
		},
		{
			name: "swap on resource disk",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.trees["/dev/sdb"].Children[0].Mountpoints = []string{swapMountpoint}
			},
			expectErr: "in use as swap",
		},
		{
			name: "resource disk backs root",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.trees["/dev/sdb"].Children[0].Mountpoints = []string{"/"}
			},
			expectErr: "mounted at /",
		},
		{
			name: "resource disk backs kubelet",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.trees["/dev/sdb"].Children[0].Mountpoints = []string{"/var/lib/kubelet"}
			},
			expectErr: "mounted at /var/lib/kubelet",
		},
		{
			name: "resource disk backs a pod mount",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.trees["/dev/sdb"].Children[0].Mountpoints = []string{mntTarget, "/var/lib/kubelet/pods/123/volumes/foo"}
			},
			expectErr: "mounted at /var/lib/kubelet/pods/123/volumes/foo",
		},
		{
			name: "foreign holder on resource disk",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.trees["/dev/sdb"].Children[0].Mountpoints = nil
				host.trees["/dev/sdb"].Children[0].Children = []Device{
					{Path: "/dev/mapper/foreign", Type: "crypt"},
				}
			},
			expectErr: "foreign holders",
		},
		{
			name: "kubelet dir beneath mnt",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.runtimePaths["/var/lib/kubelet"] = "/mnt/kubelet"
			},
			expectErr: "beneath /mnt",
		},
		{
			name: "waagent formats resource disk",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				writeHostFile(t, cfg.HostEtcDir, "waagent.conf", "ResourceDisk.Format=y\n")
			},
			expectErr: "ResourceDisk.Format=y",
		},
		{
			name: "waagent swap on resource disk",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				writeHostFile(t, cfg.HostEtcDir, "waagent.conf", "ResourceDisk.Format=n\nResourceDisk.EnableSwap=y\n")
			},
			expectErr: "ResourceDisk.EnableSwap=y",
		},
		{
			name: "submount below mnt",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.otherMounts = append(host.otherMounts, "/mnt/docker")
			},
			expectErr: "below /mnt",
		},
		{
			name: "missing host command",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.missingCommands["wipefs"] = true
			},
			expectErr: "wipefs",
		},
		{
			name: "unrecognized whole-disk filesystem",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.trees["/dev/sdb"] = blankDiskTree()
				host.trees["/dev/sdb"].FSType = "xfs"
			},
			expectErr: "unrecognized filesystem signature",
		},
		{
			name: "multiple partitions",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				host.unmountMnt()
				tree := host.trees["/dev/sdb"]
				tree.Children = append(tree.Children, Device{Path: "/dev/sdb2", Type: partType, FSType: "ext4"})
			},
			expectErr: "unrecognized partition layout",
		},
		{
			name: "destructive preparation not allowed",
			mutate: func(t *testing.T, host *fakeHost, cfg *Config) {
				cfg.AllowDestructivePreparation = false
			},
			expectErr: "destructive preparation is not allowed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host, cfg := cloudInitScenario(t, "/dev/sdb")
			tc.mutate(t, host, &cfg)

			fstabBefore := readHostFile(t, cfg.HostEtcDir, "fstab")

			err := prepare(t, host, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.expectErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.expectErr, err)
			}
			// Fail closed: no destructive command may have run and fstab
			// must be untouched.
			for _, forbidden := range []string{"wipefs", "pvcreate", "vgcreate", "umount", "systemctl stop"} {
				if calls := host.callsMatching(forbidden); len(calls) > 0 {
					t.Errorf("rejected state executed %v", calls)
				}
			}
			if got := readHostFile(t, cfg.HostEtcDir, "fstab"); got != fstabBefore {
				t.Errorf("rejected state modified fstab:\n%s", got)
			}
		})
	}
}

func TestPrepareFailureAtEachMutationStep(t *testing.T) {
	t.Parallel()
	steps := []string{
		"umount /mnt",
		"systemctl daemon-reload",
		"findmnt --verify",
		"wipefs --all /dev/sdb1",
		"wipefs --all --force /dev/sdb",
		"blockdev --rereadpt",
		"pvcreate",
		"vgcreate",
		"pvscan",
		"vgscan",
		"vgchange",
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			host, cfg := cloudInitScenario(t, "/dev/sdb")
			host.failOn[step] = fmt.Errorf("injected failure at %q", step)
			if step == "umount /mnt" {
				// The systemctl fallback would otherwise unmount /mnt.
				host.failOn["systemctl stop"] = fmt.Errorf("injected failure at systemctl stop")
			}

			err := prepare(t, host, cfg)
			if err == nil || !strings.Contains(err.Error(), "injected failure") {
				t.Fatalf("expected injected failure to propagate, got: %v", err)
			}
		})
	}
}

func TestPrepareUnmountedCloudInitPartition(t *testing.T) {
	t.Parallel()
	host, cfg := cloudInitScenario(t, "/dev/sdb")
	host.unmountMnt()

	if err := prepare(t, host, cfg); err != nil {
		t.Fatalf("Prepare() failed: %v", err)
	}
	if calls := host.callsMatching("umount"); len(calls) > 0 {
		t.Errorf("unexpected umount for an already unmounted partition: %v", calls)
	}
	if vg := host.pvs["/dev/sdb"]; vg != testVG {
		t.Errorf("expected /dev/sdb to be a PV of %s, got %q", testVG, vg)
	}
}

func TestPrepareLVMDevicesFileRegistration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		existing      string
		expectAddCall bool
	}{
		{name: "registers missing entry", existing: "DEVNAME=/dev/sda\n", expectAddCall: true},
		{name: "already registered", existing: "DEVNAME=/dev/sda\nDEVNAME=/dev/sdb\n", expectAddCall: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host, cfg := cloudInitScenario(t, "/dev/sdb")
			host.lvmDevicesOut = tc.existing
			writeHostFile(t, filepath.Join(cfg.HostEtcDir, "lvm", "devices"), "system.devices", "# LVM uses devices listed in this file\n")

			if err := prepare(t, host, cfg); err != nil {
				t.Fatalf("Prepare() failed: %v", err)
			}
			addCalls := host.callsMatching("lvmdevices --adddev /dev/sdb")
			if tc.expectAddCall && len(addCalls) != 1 {
				t.Errorf("expected one lvmdevices --adddev call, got: %v", addCalls)
			}
			if !tc.expectAddCall && len(addCalls) != 0 {
				t.Errorf("expected no lvmdevices --adddev call, got: %v", addCalls)
			}
		})
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	host := newFakeHost(t)
	if _, err := New(Config{VolumeGroup: "bad name"}, host); err == nil {
		t.Error("expected invalid volume group name error")
	}
	if _, err := New(Config{VolumeGroup: "-leading"}, host); err == nil {
		t.Error("expected invalid volume group name error for leading dash")
	}
	if _, err := New(Config{VolumeGroup: "ok"}, nil); err == nil {
		t.Error("expected nil runner error")
	}
	p, err := New(Config{VolumeGroup: "ok"}, host)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.cfg.ResourceDiskLink != DefaultResourceDiskLink || p.cfg.VolumeGroupTag != DefaultVolumeGroupTag {
		t.Errorf("defaults not applied: %+v", p.cfg)
	}
}

func writeFstab(t *testing.T, etcDir, content string) {
	t.Helper()
	writeHostFile(t, etcDir, "fstab", content)
}

func writeHostFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", name, err)
	}
}

func readHostFile(t *testing.T, dir, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("failed to read %s: %v", name, err)
	}
	return string(content)
}
