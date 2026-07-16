// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"
)

const (
	fixtureResourceDisk      = "/dev/sdb"
	fixtureResourcePartition = "/dev/sdb1"
)

func TestPreparerStopsWhenRequiredCommandIsMissing(t *testing.T) {
	t.Parallel()

	config, locker := newTestConfig(t, true)
	runner := NewMockCommandRunner(gomock.NewController(t))
	runner.EXPECT().Run(gomock.Any(), Command{
		Name: "sh",
		Args: []string{"-c", `command -v -- "$1" >/dev/null 2>&1`, "nodeprep-command-check", "blockdev"},
	}).Return(Output{}, errors.New("command not found"))

	preparer, err := NewPreparer(config, runner, locker, NewFSTabEditor(config.HostEtcPath), discardLogger())
	if err != nil {
		t.Fatalf("NewPreparer() error = %v", err)
	}
	if _, err := preparer.Prepare(context.Background()); err == nil || !strings.Contains(err.Error(), "verify required host command blockdev") {
		t.Fatalf("Prepare() error = %v, want missing command error", err)
	}
	if !locker.released {
		t.Fatal("host lock was not released after preflight failure")
	}
}

func TestPreparerSupportedLVMTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		initial       fixtureState
		wantAction    Action
		wantMutations []string
	}{
		{
			name:          "blank disk creates PV and VG",
			initial:       fixtureBlank,
			wantAction:    ActionCreatedPhysicalVolume,
			wantMutations: []string{"pvcreate", "vgcreate", "pvscan", "vgscan", "vgchange"},
		},
		{
			name:          "orphan PV creates only VG",
			initial:       fixtureOrphanPV,
			wantAction:    ActionCreatedVolumeGroup,
			wantMutations: []string{"vgcreate", "pvscan", "vgscan", "vgchange"},
		},
		{
			name:          "managed VG refreshes without wipe",
			initial:       fixtureManagedVG,
			wantAction:    ActionVerifiedVolumeGroup,
			wantMutations: []string{"pvscan", "vgscan", "vgchange"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config, locker := newTestConfig(t, true)
			runner := newFixtureRunner(locker, test.initial)
			preparer, err := NewPreparer(config, runner, locker, NewFSTabEditor(config.HostEtcPath), discardLogger())
			if err != nil {
				t.Fatalf("NewPreparer() error = %v", err)
			}

			result, err := preparer.Prepare(context.Background())
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if result.Action != test.wantAction || result.DevicePath != fixtureResourceDisk || result.MajorMinor != "8:16" {
				t.Fatalf("Prepare() result = %#v, want action %s on /dev/sdb (8:16)", result, test.wantAction)
			}
			if got := runner.mutations(); !reflect.DeepEqual(got, test.wantMutations) {
				t.Fatalf("mutations = %#v, want %#v", got, test.wantMutations)
			}
			if !locker.released {
				t.Fatal("host lock was not released")
			}
		})
	}
}

func TestPreparerRejectsIdentityChangeBeforeWrite(t *testing.T) {
	t.Parallel()

	config, locker := newTestConfig(t, true)
	runner := newFixtureRunner(locker, fixtureBlank)
	runner.changeIdentity = true
	preparer, err := NewPreparer(config, runner, locker, NewFSTabEditor(config.HostEtcPath), discardLogger())
	if err != nil {
		t.Fatalf("NewPreparer() error = %v", err)
	}

	_, err = preparer.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "identity changed from 8:16 to 8:32") {
		t.Fatalf("Prepare() error = %v, want identity change rejection", err)
	}
	if mutations := runner.mutations(); len(mutations) != 0 {
		t.Fatalf("mutations after identity change = %#v, want none", mutations)
	}
}

func TestPreparerRejectsForeignFilesystemBeforeWrite(t *testing.T) {
	t.Parallel()

	config, locker := newTestConfig(t, true)
	runner := newFixtureRunner(locker, fixtureForeignFilesystem)
	preparer, err := NewPreparer(config, runner, locker, NewFSTabEditor(config.HostEtcPath), discardLogger())
	if err != nil {
		t.Fatalf("NewPreparer() error = %v", err)
	}

	_, err = preparer.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "recognized cloud-init partition") {
		t.Fatalf("Prepare() error = %v, want foreign filesystem rejection", err)
	}
	if mutations := runner.mutations(); len(mutations) != 0 {
		t.Fatalf("mutations for foreign filesystem = %#v, want none", mutations)
	}
}

func TestPreparerRejectsActiveBlockDeviceUserBeforeWrite(t *testing.T) {
	t.Parallel()

	config, locker := newTestConfig(t, true)
	runner := newFixtureRunner(locker, fixtureBlank)
	runner.deviceInUse = true
	preparer, err := NewPreparer(config, runner, locker, NewFSTabEditor(config.HostEtcPath), discardLogger())
	if err != nil {
		t.Fatalf("NewPreparer() error = %v", err)
	}

	_, err = preparer.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "active block-device users") {
		t.Fatalf("Prepare() error = %v, want active-user rejection", err)
	}
	if mutations := runner.mutations(); len(mutations) != 0 {
		t.Fatalf("mutations for active device = %#v, want none", mutations)
	}
}

func TestPreparerRegistersAndVerifiesLVMDevicesFile(t *testing.T) {
	t.Parallel()

	config, locker := newTestConfig(t, true)
	devicesDirectory := filepath.Join(config.HostEtcPath, "lvm", "devices")
	if err := os.MkdirAll(devicesDirectory, 0755); err != nil {
		t.Fatalf("create LVM devices directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devicesDirectory, "system.devices"), nil, 0600); err != nil {
		t.Fatalf("create LVM devices file: %v", err)
	}
	runner := newFixtureRunner(locker, fixtureBlank)
	preparer, err := NewPreparer(config, runner, locker, NewFSTabEditor(config.HostEtcPath), discardLogger())
	if err != nil {
		t.Fatalf("NewPreparer() error = %v", err)
	}

	if _, err := preparer.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	wantMutations := []string{
		"pvcreate",
		"vgcreate",
		"lvmdevices:--adddev",
		"pvscan",
		"vgscan",
		"vgchange",
		"lvmdevices:--check",
	}
	if got := runner.mutations(); !reflect.DeepEqual(got, wantMutations) {
		t.Fatalf("mutations = %#v, want %#v", got, wantMutations)
	}

	foundUnrestrictedDiscovery := false
	for _, command := range runner.calls {
		if command.Name == "pvs" && contains(command.Args, "--devicesfile") && contains(command.Args, "") {
			foundUnrestrictedDiscovery = true
			break
		}
	}
	if !foundUnrestrictedDiscovery {
		t.Fatal("preflight pvs did not disable system.devices for foreign metadata discovery")
	}
}

func TestPreparerConvertsRecognizedCloudInitLayout(t *testing.T) {
	t.Parallel()

	config, locker := newTestConfig(t, true)
	fstab := "/dev/disk/cloud/azure_resource-part1 /mnt auto defaults,nofail,x-systemd.after=cloud-init-network.service,comment=cloudconfig 0 2\n"
	if err := os.WriteFile(filepath.Join(config.HostEtcPath, "fstab"), []byte(fstab), 0644); err != nil {
		t.Fatalf("write fstab fixture: %v", err)
	}
	runner := newFixtureRunner(locker, fixtureCloudInit)
	preparer, err := NewPreparer(config, runner, locker, NewFSTabEditor(config.HostEtcPath), discardLogger())
	if err != nil {
		t.Fatalf("NewPreparer() error = %v", err)
	}

	result, err := preparer.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.Action != ActionConvertedCloudInitMount {
		t.Fatalf("Prepare() action = %s, want %s", result.Action, ActionConvertedCloudInitMount)
	}
	wantMutations := []string{
		"systemctl:stop",
		"systemctl:daemon-reload",
		"wipefs:/dev/sdb1",
		"wipefs:/dev/sdb",
		"blockdev",
		"pvcreate",
		"vgcreate",
		"pvscan",
		"vgscan",
		"vgchange",
	}
	if got := runner.mutations(); !reflect.DeepEqual(got, wantMutations) {
		t.Fatalf("mutations = %#v, want %#v", got, wantMutations)
	}

	updated, err := os.ReadFile(filepath.Join(config.HostEtcPath, "fstab"))
	if err != nil {
		t.Fatalf("read updated fstab: %v", err)
	}
	if !strings.HasPrefix(string(updated), fstabCommentPrefix) {
		t.Fatalf("updated fstab = %q, want neutralized cloud-init entry", updated)
	}
	backup, err := os.ReadFile(filepath.Join(config.HostEtcPath, "fstab") + fstabBackupSuffix)
	if err != nil {
		t.Fatalf("read fstab backup: %v", err)
	}
	if string(backup) != fstab {
		t.Fatalf("fstab backup = %q, want %q", backup, fstab)
	}
}

func TestPreparerResumesNeutralizedCloudInitLayout(t *testing.T) {
	t.Parallel()

	config, locker := newTestConfig(t, true)
	original := "/dev/disk/cloud/azure_resource-part1 /mnt auto defaults,nofail,comment=cloudconfig 0 2\n"
	fstabPath := filepath.Join(config.HostEtcPath, "fstab")
	if err := os.WriteFile(fstabPath, []byte(fstabCommentPrefix+original), 0644); err != nil {
		t.Fatalf("write neutralized fstab fixture: %v", err)
	}
	if err := os.WriteFile(fstabPath+fstabBackupSuffix, []byte(original), 0644); err != nil {
		t.Fatalf("write fstab backup fixture: %v", err)
	}
	runner := newFixtureRunner(locker, fixtureCloudInit)
	runner.cloudUnmounted = true
	runner.mntUnitMissing = true
	preparer, err := NewPreparer(config, runner, locker, NewFSTabEditor(config.HostEtcPath), discardLogger())
	if err != nil {
		t.Fatalf("NewPreparer() error = %v", err)
	}

	result, err := preparer.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.Action != ActionConvertedCloudInitMount {
		t.Fatalf("Prepare() action = %s, want resumed cloud-init conversion", result.Action)
	}
}

func TestPreparerMissingResourceLinkPolicy(t *testing.T) {
	t.Parallel()

	for _, required := range []bool{false, true} {
		t.Run(map[bool]string{false: "optional", true: "required"}[required], func(t *testing.T) {
			t.Parallel()
			config, locker := newTestConfig(t, false)
			config.ResourceRequired = required
			runner := newFixtureRunner(locker, fixtureBlank)
			preparer, err := NewPreparer(config, runner, locker, NewFSTabEditor(config.HostEtcPath), discardLogger())
			if err != nil {
				t.Fatalf("NewPreparer() error = %v", err)
			}

			result, err := preparer.Prepare(context.Background())
			if required {
				if err == nil || !strings.Contains(err.Error(), "required resource link") {
					t.Fatalf("Prepare() error = %v, want required link error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if result.Action != ActionNoop {
				t.Fatalf("Prepare() action = %s, want no-op", result.Action)
			}
			if mutations := runner.mutations(); len(mutations) != 0 {
				t.Fatalf("mutations without resource link = %#v, want none", mutations)
			}
		})
	}
}

type fixtureState int

const (
	fixtureBlank fixtureState = iota
	fixtureOrphanPV
	fixtureManagedVG
	fixtureForeignFilesystem
	fixtureCloudInit
)

type recordingLocker struct {
	held     bool
	released bool
}

func (l *recordingLocker) Acquire(context.Context) (Lock, error) {
	if l.held {
		return nil, errors.New("test lock already held")
	}
	l.held = true
	return recordingLock{locker: l}, nil
}

type recordingLock struct {
	locker *recordingLocker
}

func (l recordingLock) Release() error {
	l.locker.held = false
	l.locker.released = true
	return nil
}

type fixtureRunner struct {
	locker         *recordingLocker
	initial        fixtureState
	calls          []Command
	identityReads  int
	changeIdentity bool
	createdVG      bool
	wiped          bool
	cloudUnmounted bool
	deviceInUse    bool
	mntUnitMissing bool
}

func newFixtureRunner(locker *recordingLocker, initial fixtureState) *fixtureRunner {
	return &fixtureRunner{locker: locker, initial: initial}
}

func (r *fixtureRunner) Run(_ context.Context, command Command) (Output, error) {
	if !r.locker.held {
		return Output{}, errors.New("host command executed without the host lock")
	}
	command.Args = append([]string(nil), command.Args...)
	r.calls = append(r.calls, command)

	switch command.Name {
	case "sh", "cloud-init", "udevadm", "pvscan", "vgscan", "vgchange", "blockdev", "lvmdevices":
		return Output{}, nil
	case "systemctl":
		if r.mntUnitMissing && reflect.DeepEqual(command.Args, []string{"stop", "mnt.mount"}) {
			return Output{}, exitCodeError(command, 5)
		}
		return Output{}, nil
	case "readlink":
		return r.readlink(command)
	case "lsblk":
		return r.lsblk(command)
	case "findmnt":
		return r.findmnt(command)
	case "fuser":
		if r.deviceInUse && contains(command.Args, fixtureResourceDisk) {
			return Output{}, nil
		}
		return Output{}, exitCodeError(command, 1)
	case "swapon":
		return Output{}, nil
	case "pvs":
		return Output{Stdout: []byte(r.pvsReport())}, nil
	case "vgs":
		return Output{Stdout: []byte(r.vgsReport())}, nil
	case "pvcreate":
		return Output{}, nil
	case "vgcreate":
		r.createdVG = true
		return Output{}, nil
	case "wipefs":
		if contains(command.Args, "--json") {
			return Output{Stdout: []byte(`{"signatures":[]}`)}, nil
		}
		if contains(command.Args, "--force") {
			r.wiped = true
		}
		return Output{}, nil
	default:
		return Output{}, errors.New("unexpected test command: " + command.Name)
	}
}

func (r *fixtureRunner) readlink(command Command) (Output, error) {
	if len(command.Args) != 2 {
		return Output{}, errors.New("unexpected readlink arguments")
	}
	path := command.Args[1]
	if command.Args[0] == "--canonicalize-missing" {
		return Output{Stdout: []byte(path + "\n")}, nil
	}
	switch path {
	case "/dev/disk/cloud/azure_resource-part1", fixtureResourcePartition:
		return Output{Stdout: []byte(fixtureResourcePartition + "\n")}, nil
	case fixtureResourceDisk:
		return Output{Stdout: []byte(fixtureResourceDisk + "\n")}, nil
	default:
		return Output{Stdout: []byte(fixtureResourceDisk + "\n")}, nil
	}
}

func (r *fixtureRunner) lsblk(command Command) (Output, error) {
	if contains(command.Args, "--nodeps") {
		r.identityReads++
		identity := "8:16"
		if r.changeIdentity && r.identityReads > 1 {
			identity = "8:32"
		}
		return Output{Stdout: []byte(singleDiskJSON(identity, "", "", nil))}, nil
	}
	if contains(command.Args, "--inverse") {
		children := []BlockDevice(nil)
		if r.initial == fixtureManagedVG {
			children = []BlockDevice{{Name: "/dev/mapper/containerstorage-lv", Path: "/dev/mapper/containerstorage-lv", MajorMinor: "253:0", Type: deviceTypeLVM}}
		}
		return Output{Stdout: []byte(singleDiskJSON("8:16", "", "", children))}, nil
	}

	if r.wiped {
		return Output{Stdout: []byte(singleDiskJSON("8:16", "", "", nil))}, nil
	}
	switch r.initial {
	case fixtureCloudInit:
		partition := BlockDevice{
			Name:        fixtureResourcePartition,
			Path:        fixtureResourcePartition,
			KernelName:  fixtureResourcePartition,
			ParentName:  fixtureResourceDisk,
			MajorMinor:  "8:17",
			Type:        deviceTypePart,
			Filesystem:  "ext4",
			Mountpoints: nullableStrings{"/mnt"},
		}
		return Output{Stdout: []byte(singleDiskJSON("8:16", "", "dos", []BlockDevice{partition}))}, nil
	case fixtureOrphanPV, fixtureManagedVG:
		return Output{Stdout: []byte(singleDiskJSON("8:16", "LVM2_member", "", nil))}, nil
	case fixtureForeignFilesystem:
		return Output{Stdout: []byte(singleDiskJSON("8:16", "ext4", "", nil))}, nil
	default:
		return Output{Stdout: []byte(singleDiskJSON("8:16", "", "", nil))}, nil
	}
}

func (r *fixtureRunner) findmnt(command Command) (Output, error) {
	if contains(command.Args, "--json") {
		if r.initial == fixtureCloudInit && !r.cloudUnmounted {
			return Output{Stdout: []byte(`{"filesystems":[{"source":"/dev/sdb1","target":"/mnt","maj:min":"8:17","fstype":"ext4","options":"rw"}]}`)}, nil
		}
		return Output{Stdout: []byte(`{"filesystems":[]}`)}, nil
	}
	if contains(command.Args, "--verify") {
		return Output{}, nil
	}
	if contains(command.Args, "--mountpoint") {
		return Output{}, exitCodeError(command, 1)
	}
	return Output{}, errors.New("unexpected findmnt arguments")
}

func (r *fixtureRunner) pvsReport() string {
	if r.createdVG || r.initial == fixtureManagedVG {
		return `{"report":[{"pv":[{"pv_name":"` + fixtureResourceDisk + `","vg_name":"containerstorage"}]}]}`
	}
	if r.initial == fixtureOrphanPV {
		return `{"report":[{"pv":[{"pv_name":"` + fixtureResourceDisk + `","vg_name":""}]}]}`
	}
	return `{"report":[{"pv":[]}]}`
}

func (r *fixtureRunner) vgsReport() string {
	if r.createdVG || r.initial == fixtureManagedVG {
		return `{"report":[{"vg":[{"vg_name":"containerstorage","vg_tags":"local-csi"}]}]}`
	}
	return `{"report":[{"vg":[]}]}`
}

func (r *fixtureRunner) mutations() []string {
	var result []string
	for _, command := range r.calls {
		switch command.Name {
		case "pvcreate", "vgcreate", "pvscan", "vgscan", "vgchange", "blockdev":
			result = append(result, command.Name)
		case "lvmdevices":
			result = append(result, "lvmdevices:"+command.Args[0])
		case "systemctl":
			result = append(result, "systemctl:"+command.Args[0])
		case "wipefs":
			if !contains(command.Args, "--json") {
				result = append(result, "wipefs:"+command.Args[len(command.Args)-1])
			}
		}
	}
	return result
}

func newTestConfig(t *testing.T, resourceExists bool) (Config, *recordingLocker) {
	t.Helper()
	hostEtc := t.TempDir()
	resourceLink := filepath.Join(t.TempDir(), "resource")
	if resourceExists {
		if err := os.WriteFile(resourceLink, nil, 0600); err != nil {
			t.Fatalf("create resource link fixture: %v", err)
		}
	}
	config := DefaultConfig()
	config.ResourceLink = resourceLink
	config.ResourceRequired = true
	config.HostEtcPath = hostEtc
	config.CloudInitTimeout = time.Second
	return config, &recordingLocker{}
}

func singleDiskJSON(identity, filesystem, partitionTable string, children []BlockDevice) string {
	device := BlockDevice{
		Name:           fixtureResourceDisk,
		Path:           fixtureResourceDisk,
		KernelName:     fixtureResourceDisk,
		MajorMinor:     identity,
		Type:           deviceTypeDisk,
		Filesystem:     filesystem,
		PartitionTable: partitionTable,
		Children:       children,
	}
	data, err := json.Marshal(lsblkReport{Devices: []BlockDevice{device}})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func exitCodeError(command Command, code int) error {
	return &CommandError{Command: command, ExitCode: code, Err: errors.New("exit status")}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
