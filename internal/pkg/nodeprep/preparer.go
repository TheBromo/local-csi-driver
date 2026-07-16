// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package nodeprep safely prepares an Azure resource disk for local-csi.
package nodeprep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	DefaultResourceLink     = "/dev/disk/azure/resource"
	DefaultVolumeGroup      = "containerstorage"
	DefaultVolumeGroupTag   = "local-csi"
	DefaultHostEtcPath      = "/host/etc"
	DefaultLockPath         = "/proc/1/root/run/lock/local-csi-nodeprep.lock"
	DefaultCloudInitTimeout = 5 * time.Minute
)

var (
	requiredHostCommands = []string{
		"blockdev",
		"cloud-init",
		"findfs",
		"findmnt",
		"fuser",
		"lsblk",
		"pvcreate",
		"pvs",
		"pvscan",
		"readlink",
		"sh",
		"swapon",
		"systemctl",
		"udevadm",
		"vgchange",
		"vgcreate",
		"vgs",
		"vgscan",
		"wipefs",
	}

	protectedRuntimePaths = []string{
		"/var/lib/kubelet",
		"/var/lib/containerd",
		"/var/lib/docker",
	}
)

// Action is the state transition completed by Prepare.
type Action string

const (
	ActionNoop                    Action = "no-op"
	ActionVerifiedVolumeGroup     Action = "verified-existing-volume-group"
	ActionCreatedVolumeGroup      Action = "created-volume-group-from-existing-pv"
	ActionCreatedPhysicalVolume   Action = "created-pv-and-volume-group"
	ActionConvertedCloudInitMount Action = "converted-cloud-init-resource-disk"
)

// Result describes the completed preparation operation.
type Result struct {
	Action      Action
	DevicePath  string
	MajorMinor  string
	VolumeGroup string
}

// Config controls resource-disk preparation.
type Config struct {
	ResourceLink     string
	ResourceRequired bool
	VolumeGroup      string
	VolumeGroupTag   string
	HostEtcPath      string
	CloudInitTimeout time.Duration
}

// DefaultConfig returns production-safe defaults. ResourceRequired is false so
// nodes without an Azure resource link remain a no-op.
func DefaultConfig() Config {
	return Config{
		ResourceLink:     DefaultResourceLink,
		ResourceRequired: false,
		VolumeGroup:      DefaultVolumeGroup,
		VolumeGroupTag:   DefaultVolumeGroupTag,
		HostEtcPath:      DefaultHostEtcPath,
		CloudInitTimeout: DefaultCloudInitTimeout,
	}
}

// Preparer coordinates host discovery, validation, and state transitions.
type Preparer struct {
	config Config
	runner CommandRunner
	locker Locker
	fstab  *FSTabEditor
	logger *slog.Logger
}

// NewPreparer validates dependencies and constructs a Preparer.
func NewPreparer(config Config, runner CommandRunner, locker Locker, fstab *FSTabEditor, logger *slog.Logger) (*Preparer, error) {
	if runner == nil {
		return nil, fmt.Errorf("command runner is required")
	}
	if locker == nil {
		return nil, fmt.Errorf("host locker is required")
	}
	if fstab == nil {
		return nil, fmt.Errorf("fstab editor is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}

	return &Preparer{config: config, runner: runner, locker: locker, fstab: fstab, logger: logger}, nil
}

func validateConfig(config Config) error {
	if config.ResourceLink == "" || !filepath.IsAbs(config.ResourceLink) {
		return fmt.Errorf("resource link must be an absolute path")
	}
	if config.VolumeGroup == "" {
		return fmt.Errorf("volume group cannot be empty")
	}
	if config.VolumeGroupTag == "" {
		return fmt.Errorf("volume group tag cannot be empty")
	}
	if config.HostEtcPath == "" || !filepath.IsAbs(config.HostEtcPath) {
		return fmt.Errorf("host etc path must be absolute")
	}
	if config.CloudInitTimeout <= 0 {
		return fmt.Errorf("cloud-init timeout must be positive")
	}
	return nil
}

type observedState int

const (
	stateExistingVolumeGroup observedState = iota
	stateOrphanPhysicalVolume
	stateBlankDisk
	stateCloudInitMount
)

type observation struct {
	state          observedState
	device         BlockDevice
	graph          *DeviceGraph
	resourceMount  *BlockDevice
	fstabPlan      *FSTabPlan
	usesLVMDevices bool
}

// Prepare performs all preflight checks and applies the one supported state
// transition. The host-wide lock is held until verification completes.
func (p *Preparer) Prepare(ctx context.Context) (result Result, returnErr error) {
	lock, err := p.locker.Acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()

	usesLVMDevices, err := p.usesLVMDevices()
	if err != nil {
		return Result{}, err
	}
	if err := p.verifyRequiredCommands(ctx, usesLVMDevices); err != nil {
		return Result{}, err
	}
	if err := p.waitForCloudInit(ctx); err != nil {
		return Result{}, err
	}
	if _, err := p.run(ctx, "settle udev before resource discovery", Command{Name: "udevadm", Args: []string{"settle"}}); err != nil {
		return Result{}, err
	}

	if _, err := os.Stat(p.config.ResourceLink); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if p.config.ResourceRequired {
				return Result{}, fmt.Errorf("required resource link %s does not exist", p.config.ResourceLink)
			}
			p.logger.Info("Azure resource link is absent; leaving existing NVMe discovery unchanged", "path", p.config.ResourceLink)
			return Result{Action: ActionNoop, VolumeGroup: p.config.VolumeGroup}, nil
		}
		return Result{}, fmt.Errorf("stat resource link %s: %w", p.config.ResourceLink, err)
	}

	observed, err := p.observe(ctx, usesLVMDevices)
	if err != nil {
		return Result{}, err
	}

	result = Result{
		DevicePath:  observed.device.Path,
		MajorMinor:  observed.device.MajorMinor,
		VolumeGroup: p.config.VolumeGroup,
	}

	switch observed.state {
	case stateExistingVolumeGroup:
		if err := p.refreshAndVerify(ctx, observed.device.Path, usesLVMDevices); err != nil {
			return Result{}, err
		}
		result.Action = ActionVerifiedVolumeGroup
	case stateOrphanPhysicalVolume:
		if err := p.confirmIdentity(ctx, observed.device); err != nil {
			return Result{}, err
		}
		if _, err := p.run(ctx, "create volume group from existing physical volume", Command{
			Name: "vgcreate",
			Args: []string{"--addtag", p.config.VolumeGroupTag, p.config.VolumeGroup, observed.device.Path},
		}); err != nil {
			return Result{}, err
		}
		if err := p.refreshAndVerify(ctx, observed.device.Path, usesLVMDevices); err != nil {
			return Result{}, err
		}
		result.Action = ActionCreatedVolumeGroup
	case stateBlankDisk:
		if err := p.confirmIdentity(ctx, observed.device); err != nil {
			return Result{}, err
		}
		if err := p.createPhysicalVolumeAndGroup(ctx, observed.device.Path, usesLVMDevices); err != nil {
			return Result{}, err
		}
		result.Action = ActionCreatedPhysicalVolume
	case stateCloudInitMount:
		if err := p.convertCloudInitDisk(ctx, observed); err != nil {
			return Result{}, err
		}
		result.Action = ActionConvertedCloudInitMount
	default:
		return Result{}, fmt.Errorf("unsupported observed state %d", observed.state)
	}

	return result, nil
}

func (p *Preparer) usesLVMDevices() (bool, error) {
	path := filepath.Join(p.config.HostEtcPath, "lvm", "devices", "system.devices")
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat host LVM devices file: %w", err)
}

func (p *Preparer) verifyRequiredCommands(ctx context.Context, usesLVMDevices bool) error {
	commands := append([]string(nil), requiredHostCommands...)
	if usesLVMDevices {
		commands = append(commands, "lvmdevices")
	}
	for _, name := range commands {
		if _, err := p.run(ctx, "verify required host command "+name, Command{
			Name: "sh",
			Args: []string{"-c", `command -v -- "$1" >/dev/null 2>&1`, "nodeprep-command-check", name},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (p *Preparer) waitForCloudInit(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, p.config.CloudInitTimeout)
	defer cancel()
	_, err := p.run(waitCtx, "wait for cloud-init", Command{Name: "cloud-init", Args: []string{"status", "--wait"}})
	return err
}

func (p *Preparer) observe(ctx context.Context, usesLVMDevices bool) (*observation, error) {
	device, err := p.resolveResourceDevice(ctx)
	if err != nil {
		return nil, err
	}
	graph, err := p.loadDeviceGraph(ctx, device)
	if err != nil {
		return nil, err
	}

	mountOutput, err := p.run(ctx, "list host mounts", Command{
		Name: "findmnt",
		Args: []string{"--json", "--evaluate", "--output", "SOURCE,TARGET,MAJ:MIN,FSTYPE,OPTIONS"},
	})
	if err != nil {
		return nil, err
	}
	mounts, err := parseMounts(mountOutput.Stdout)
	if err != nil {
		return nil, err
	}
	resourceMount, err := validateCandidateMounts(graph, mounts)
	if err != nil {
		return nil, err
	}

	if err := p.validateRuntimePaths(ctx); err != nil {
		return nil, err
	}
	if err := p.validateSwap(ctx, graph); err != nil {
		return nil, err
	}
	if err := validateWAAgentConfig(filepath.Join(p.config.HostEtcPath, "waagent.conf")); err != nil {
		return nil, err
	}

	pvs, vgs, err := p.discoverLVM(ctx, usesLVMDevices)
	if err != nil {
		return nil, err
	}
	state, plan, err := p.classify(ctx, device, graph, resourceMount, pvs, vgs)
	if err != nil {
		return nil, err
	}
	if state == stateBlankDisk || state == stateOrphanPhysicalVolume {
		if err := p.validateBlockDeviceUnused(ctx, graph); err != nil {
			return nil, err
		}
	}
	if state == stateCloudInitMount && resourceMount == nil {
		partitions := graph.directPartitions()
		resourceMount = &partitions[0]
	}

	return &observation{
		state:          state,
		device:         device,
		graph:          graph,
		resourceMount:  resourceMount,
		fstabPlan:      plan,
		usesLVMDevices: usesLVMDevices,
	}, nil
}

func (p *Preparer) resolveResourceDevice(ctx context.Context) (BlockDevice, error) {
	resolved, err := p.canonicalizeExisting(ctx, p.config.ResourceLink)
	if err != nil {
		return BlockDevice{}, fmt.Errorf("resolve resource link %s: %w", p.config.ResourceLink, err)
	}
	if !filepath.IsAbs(resolved) || !pathWithin(resolved, "/dev") {
		return BlockDevice{}, fmt.Errorf("resource link resolved outside /dev: %s", resolved)
	}

	output, err := p.run(ctx, "read resource disk identity", lsblkIdentityCommand(resolved))
	if err != nil {
		return BlockDevice{}, err
	}
	device, err := parseSingleDevice(output.Stdout)
	if err != nil {
		return BlockDevice{}, fmt.Errorf("parse resource disk identity: %w", err)
	}
	if device.Type != deviceTypeDisk {
		return BlockDevice{}, fmt.Errorf("resource link %s resolves to %s %s, expected a whole disk", p.config.ResourceLink, device.Type, device.Path)
	}
	if filepath.Clean(device.Path) != filepath.Clean(resolved) {
		return BlockDevice{}, fmt.Errorf("lsblk canonical path %s does not match resolved resource path %s", device.Path, resolved)
	}
	if device.MajorMinor == "" {
		return BlockDevice{}, fmt.Errorf("resource disk %s has no major/minor identity", resolved)
	}
	return device, nil
}

func (p *Preparer) loadDeviceGraph(ctx context.Context, device BlockDevice) (*DeviceGraph, error) {
	children, err := p.run(ctx, "build resource disk child graph", lsblkGraphCommand(device.Path, false))
	if err != nil {
		return nil, err
	}
	holders, err := p.run(ctx, "build resource disk holder graph", lsblkGraphCommand(device.Path, true))
	if err != nil {
		return nil, err
	}
	return parseDeviceGraph(children.Stdout, holders.Stdout, device.MajorMinor)
}

func lsblkIdentityCommand(path string) Command {
	return Command{
		Name: "lsblk",
		Args: []string{"--json", "--bytes", "--paths", "--nodeps", "--output", "NAME,PATH,KNAME,PKNAME,MAJ:MIN,TYPE,FSTYPE,PTTYPE,MOUNTPOINTS", path},
	}
}

func lsblkGraphCommand(path string, inverse bool) Command {
	args := []string{"--json", "--bytes", "--paths", "--tree", "--merge"}
	if inverse {
		args = append(args, "--inverse")
	}
	args = append(args, "--output", "NAME,PATH,KNAME,PKNAME,MAJ:MIN,TYPE,FSTYPE,PTTYPE,MOUNTPOINTS", path)
	return Command{Name: "lsblk", Args: args}
}

func (p *Preparer) validateRuntimePaths(ctx context.Context) error {
	for _, path := range protectedRuntimePaths {
		output, err := p.run(ctx, "resolve protected runtime path "+path, Command{
			Name: "readlink",
			Args: []string{"--canonicalize-missing", path},
		})
		if err != nil {
			return err
		}
		resolved := strings.TrimSpace(string(output.Stdout))
		if pathWithin(resolved, "/mnt") {
			return fmt.Errorf("protected runtime path %s resolves beneath /mnt: %s", path, resolved)
		}
	}
	return nil
}

func (p *Preparer) validateSwap(ctx context.Context, graph *DeviceGraph) error {
	output, err := p.run(ctx, "list active swap", Command{
		Name: "swapon",
		Args: []string{"--show", "--noheadings", "--raw", "--output", "NAME"},
	})
	if err != nil {
		return err
	}
	for _, source := range strings.Fields(string(output.Stdout)) {
		canonical, err := p.canonicalizeExisting(ctx, source)
		if err != nil {
			return fmt.Errorf("resolve swap source %s: %w", source, err)
		}
		if pathWithin(canonical, "/mnt") || graph.containsPath(canonical) {
			return fmt.Errorf("resource disk backs active swap %s", source)
		}
	}
	return nil
}

func (p *Preparer) validateBlockDeviceUnused(ctx context.Context, graph *DeviceGraph) error {
	paths := make([]string, 0, len(graph.Candidates))
	for _, device := range graph.Candidates {
		if device.Path != "" {
			paths = append(paths, device.Path)
		}
	}
	sort.Strings(paths)
	if _, err := p.runner.Run(ctx, Command{Name: "fuser", Args: append([]string{"--silent"}, paths...)}); err == nil {
		return fmt.Errorf("resource disk has active block-device users")
	} else if !IsExitCode(err, 1) {
		return fmt.Errorf("verify resource disk users: %w", err)
	}
	return nil
}

func (p *Preparer) discoverLVM(ctx context.Context, usesLVMDevices bool) ([]physicalVolume, []volumeGroup, error) {
	extraArgs := []string{}
	if usesLVMDevices {
		extraArgs = []string{"--devicesfile", ""}
	}
	return p.listLVM(ctx, extraArgs)
}

func (p *Preparer) listLVM(ctx context.Context, extraArgs []string) ([]physicalVolume, []volumeGroup, error) {
	pvArgs := []string{"--reportformat", "json", "--options", "pv_name,vg_name"}
	pvArgs = append(pvArgs, extraArgs...)
	pvOutput, err := p.run(ctx, "list physical volumes", Command{Name: "pvs", Args: pvArgs})
	if err != nil {
		return nil, nil, err
	}
	pvs, err := parsePhysicalVolumes(pvOutput.Stdout)
	if err != nil {
		return nil, nil, err
	}
	for index := range pvs {
		canonical, err := p.canonicalizeExisting(ctx, strings.TrimSpace(pvs[index].Name))
		if err != nil {
			return nil, nil, fmt.Errorf("resolve physical volume %s: %w", pvs[index].Name, err)
		}
		pvs[index].Name = canonical
		pvs[index].VolumeGroup = strings.TrimSpace(pvs[index].VolumeGroup)
	}

	vgArgs := []string{"--reportformat", "json", "--options", "vg_name,vg_tags"}
	vgArgs = append(vgArgs, extraArgs...)
	vgOutput, err := p.run(ctx, "list volume groups", Command{Name: "vgs", Args: vgArgs})
	if err != nil {
		return nil, nil, err
	}
	vgs, err := parseVolumeGroups(vgOutput.Stdout)
	if err != nil {
		return nil, nil, err
	}
	return pvs, vgs, nil
}

func (p *Preparer) classify(
	ctx context.Context,
	device BlockDevice,
	graph *DeviceGraph,
	resourceMount *BlockDevice,
	pvs []physicalVolume,
	vgs []volumeGroup,
) (observedState, *FSTabPlan, error) {
	var expectedVG *volumeGroup
	for index := range vgs {
		if strings.TrimSpace(vgs[index].Name) == p.config.VolumeGroup {
			expectedVG = &vgs[index]
			break
		}
	}

	var resourcePV *physicalVolume
	for index := range pvs {
		if filepath.Clean(pvs[index].Name) == filepath.Clean(device.Path) {
			resourcePV = &pvs[index]
			break
		}
	}

	if expectedVG != nil {
		if !hasTag(expectedVG.Tags, p.config.VolumeGroupTag) {
			return 0, nil, fmt.Errorf("expected volume group %s exists without tag %s", p.config.VolumeGroup, p.config.VolumeGroupTag)
		}
		if resourcePV == nil || resourcePV.VolumeGroup != p.config.VolumeGroup {
			return 0, nil, fmt.Errorf("expected volume group %s exists on another disk", p.config.VolumeGroup)
		}
		if err := graph.rejectForeignHolders(true); err != nil {
			return 0, nil, err
		}
		return stateExistingVolumeGroup, nil, nil
	}

	if resourcePV != nil {
		if resourcePV.VolumeGroup != "" {
			return 0, nil, fmt.Errorf("resource disk belongs to foreign volume group %s", resourcePV.VolumeGroup)
		}
		if len(graph.Disk.Children) != 0 {
			return 0, nil, fmt.Errorf("orphan physical volume %s also has child devices", device.Path)
		}
		if err := graph.rejectForeignHolders(false); err != nil {
			return 0, nil, err
		}
		return stateOrphanPhysicalVolume, nil, nil
	}

	if graph.isBlank() {
		return stateBlankDisk, nil, nil
	}

	if err := graph.rejectForeignHolders(false); err != nil {
		return 0, nil, err
	}
	partitions := graph.directPartitions()
	if len(graph.Disk.Children) != 1 || len(partitions) != 1 {
		return 0, nil, fmt.Errorf("resource disk does not contain exactly the recognized cloud-init partition")
	}
	partition := partitions[0]
	if resourceMount != nil && resourceMount.MajorMinor != partition.MajorMinor {
		return 0, nil, fmt.Errorf("resource disk mount does not use the recognized cloud-init partition")
	}
	if graph.Disk.Filesystem != "" || graph.Disk.PartitionTable == "" || partition.Filesystem == "" || partition.Filesystem == "swap" || partition.Filesystem == "LVM2_member" {
		return 0, nil, fmt.Errorf("resource disk signatures do not match the recognized cloud-init layout")
	}
	if len(partition.Children) != 0 {
		return 0, nil, fmt.Errorf("recognized resource partition has child devices")
	}

	plan, err := p.fstab.PlanCloudInitMount(partition.Path, func(source string) (string, error) {
		return p.resolveFSTabSource(ctx, source)
	})
	if err != nil {
		return 0, nil, err
	}
	return stateCloudInitMount, plan, nil
}

func (p *Preparer) resolveFSTabSource(ctx context.Context, source string) (string, error) {
	path := source
	if !filepath.IsAbs(path) {
		output, err := p.run(ctx, "evaluate fstab source", Command{Name: "findfs", Args: []string{source}})
		if err != nil {
			return "", err
		}
		path = strings.TrimSpace(string(output.Stdout))
	}
	return p.canonicalizeExisting(ctx, path)
}

func (p *Preparer) canonicalizeExisting(ctx context.Context, path string) (string, error) {
	output, err := p.run(ctx, "canonicalize host path "+path, Command{
		Name: "readlink",
		Args: []string{"--canonicalize-existing", path},
	})
	if err != nil {
		return "", err
	}
	canonical := strings.TrimSpace(string(output.Stdout))
	if canonical == "" {
		return "", fmt.Errorf("canonical path for %s is empty", path)
	}
	return canonical, nil
}

func (p *Preparer) confirmIdentity(ctx context.Context, original BlockDevice) error {
	current, err := p.resolveResourceDevice(ctx)
	if err != nil {
		return err
	}
	if current.MajorMinor != original.MajorMinor {
		return fmt.Errorf("resource link identity changed from %s to %s", original.MajorMinor, current.MajorMinor)
	}
	return nil
}

func (p *Preparer) createPhysicalVolumeAndGroup(ctx context.Context, devicePath string, usesLVMDevices bool) error {
	if _, err := p.run(ctx, "create physical volume", Command{Name: "pvcreate", Args: []string{"--yes", devicePath}}); err != nil {
		return err
	}
	if _, err := p.run(ctx, "create volume group", Command{
		Name: "vgcreate",
		Args: []string{"--addtag", p.config.VolumeGroupTag, p.config.VolumeGroup, devicePath},
	}); err != nil {
		return err
	}
	return p.refreshAndVerify(ctx, devicePath, usesLVMDevices)
}

func (p *Preparer) refreshAndVerify(ctx context.Context, devicePath string, usesLVMDevices bool) error {
	if usesLVMDevices {
		if _, err := p.run(ctx, "register resource disk in LVM devices file", Command{Name: "lvmdevices", Args: []string{"--adddev", devicePath}}); err != nil {
			return err
		}
	}
	for _, step := range []struct {
		description string
		command     Command
	}{
		{description: "refresh physical volume cache", command: Command{Name: "pvscan", Args: []string{"--cache"}}},
		{description: "refresh volume group nodes", command: Command{Name: "vgscan", Args: []string{"--mknodes"}}},
		{description: "activate expected volume group", command: Command{Name: "vgchange", Args: []string{"--activate", "y", p.config.VolumeGroup}}},
	} {
		if _, err := p.run(ctx, step.description, step.command); err != nil {
			return err
		}
	}

	pvs, vgs, err := p.listLVM(ctx, nil)
	if err != nil {
		return err
	}
	if err := verifyLVMState(devicePath, p.config.VolumeGroup, p.config.VolumeGroupTag, pvs, vgs); err != nil {
		return err
	}
	if usesLVMDevices {
		if _, err := p.run(ctx, "verify LVM devices file", Command{Name: "lvmdevices", Args: []string{"--check"}}); err != nil {
			return err
		}
	}
	return nil
}

func verifyLVMState(devicePath, expectedVG, expectedTag string, pvs []physicalVolume, vgs []volumeGroup) error {
	pvFound := false
	for _, pv := range pvs {
		if filepath.Clean(pv.Name) == filepath.Clean(devicePath) && pv.VolumeGroup == expectedVG {
			pvFound = true
			break
		}
	}
	if !pvFound {
		return fmt.Errorf("ordinary pvs does not report %s as a member of %s", devicePath, expectedVG)
	}

	for _, vg := range vgs {
		if strings.TrimSpace(vg.Name) == expectedVG {
			if !hasTag(vg.Tags, expectedTag) {
				return fmt.Errorf("ordinary vgs reports %s without tag %s", expectedVG, expectedTag)
			}
			return nil
		}
	}
	return fmt.Errorf("ordinary vgs does not report expected volume group %s", expectedVG)
}

func (p *Preparer) convertCloudInitDisk(ctx context.Context, observed *observation) error {
	if observed.fstabPlan == nil || observed.resourceMount == nil {
		return fmt.Errorf("cloud-init conversion is missing its validated ownership plan")
	}
	if _, err := p.run(ctx, "stop generated mnt.mount", Command{Name: "systemctl", Args: []string{"stop", "mnt.mount"}}); err != nil {
		if unmountedErr := p.verifyMntUnmounted(ctx); unmountedErr != nil {
			return errors.Join(err, unmountedErr)
		}
		p.logger.Debug("mnt.mount is already absent and /mnt is unmounted")
	}
	if err := p.verifyMntUnused(ctx); err != nil {
		return err
	}
	if err := p.fstab.Apply(observed.fstabPlan); err != nil {
		return err
	}
	if _, err := p.run(ctx, "reload systemd after fstab edit", Command{Name: "systemctl", Args: []string{"daemon-reload"}}); err != nil {
		return err
	}
	if _, err := p.run(ctx, "verify host fstab", Command{Name: "findmnt", Args: []string{"--verify", "--verbose"}}); err != nil {
		return err
	}
	if err := p.verifyMntUnmounted(ctx); err != nil {
		return err
	}

	if err := p.confirmIdentity(ctx, observed.device); err != nil {
		return err
	}
	for _, partitionPath := range sortedDevicePaths(observed.graph.directPartitions()) {
		if _, err := p.run(ctx, "wipe recognized resource partition", Command{Name: "wipefs", Args: []string{"--all", partitionPath}}); err != nil {
			return err
		}
	}
	if _, err := p.run(ctx, "wipe resource disk", Command{Name: "wipefs", Args: []string{"--all", "--force", observed.device.Path}}); err != nil {
		return err
	}
	if _, err := p.run(ctx, "reread resource disk partition table", Command{Name: "blockdev", Args: []string{"--rereadpt", observed.device.Path}}); err != nil {
		return err
	}
	if _, err := p.run(ctx, "settle udev after resource disk wipe", Command{Name: "udevadm", Args: []string{"settle"}}); err != nil {
		return err
	}
	if err := p.verifyWiped(ctx, observed.device); err != nil {
		return err
	}
	return p.createPhysicalVolumeAndGroup(ctx, observed.device.Path, observed.usesLVMDevices)
}

func (p *Preparer) verifyMntUnused(ctx context.Context) error {
	if _, err := p.runner.Run(ctx, Command{Name: "fuser", Args: []string{"--mount", "--ismountpoint", "/mnt"}}); err == nil {
		return fmt.Errorf("/mnt still has active users")
	} else if !IsExitCode(err, 1) {
		return fmt.Errorf("verify /mnt users: %w", err)
	}

	if _, err := p.runner.Run(ctx, Command{Name: "findmnt", Args: []string{"--submounts", "--mountpoint", "/mnt"}}); err == nil {
		return fmt.Errorf("/mnt still has active or recursive mounts")
	} else if !IsExitCode(err, 1) {
		return fmt.Errorf("verify recursive /mnt mounts: %w", err)
	}
	return nil
}

func (p *Preparer) verifyMntUnmounted(ctx context.Context) error {
	if _, err := p.runner.Run(ctx, Command{Name: "findmnt", Args: []string{"--mountpoint", "/mnt"}}); err == nil {
		return fmt.Errorf("/mnt remains mounted after fstab neutralization")
	} else if !IsExitCode(err, 1) {
		return fmt.Errorf("verify /mnt is unmounted: %w", err)
	}
	return nil
}

func (p *Preparer) verifyWiped(ctx context.Context, original BlockDevice) error {
	output, err := p.run(ctx, "verify wiped resource disk topology", lsblkGraphCommand(original.Path, false))
	if err != nil {
		return err
	}
	report, err := parseLSBLK(output.Stdout)
	if err != nil {
		return err
	}
	device, found := findDeviceByIdentity(report.Devices, original.MajorMinor)
	if !found {
		return fmt.Errorf("wiped resource disk identity %s disappeared", original.MajorMinor)
	}
	if device.Filesystem != "" || device.PartitionTable != "" || len(device.Children) != 0 {
		return fmt.Errorf("resource disk still has signatures or child devices after wipe")
	}

	wipeOutput, err := p.run(ctx, "verify wiped resource disk signatures", Command{Name: "wipefs", Args: []string{"--json", original.Path}})
	if err != nil {
		return err
	}
	var reportJSON struct {
		Signatures []json.RawMessage `json:"signatures"`
	}
	if err := json.Unmarshal(wipeOutput.Stdout, &reportJSON); err != nil {
		return fmt.Errorf("decode wipefs verification JSON: %w", err)
	}
	if len(reportJSON.Signatures) != 0 {
		return fmt.Errorf("resource disk still has %d signatures after wipe", len(reportJSON.Signatures))
	}
	return nil
}

func (p *Preparer) run(ctx context.Context, description string, command Command) (Output, error) {
	output, err := p.runner.Run(ctx, command)
	if err != nil {
		return output, fmt.Errorf("%s: %w", description, err)
	}
	return output, nil
}
