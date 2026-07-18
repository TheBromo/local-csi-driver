package nodeprep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Preparer prepares the Azure resource disk for LVM use.
type Preparer struct {
	cfg  Config
	host Runner
}

// New creates a Preparer.
func New(cfg Config, host Runner) (*Preparer, error) {
	cfg = cfg.withDefaults()
	if host == nil {
		return nil, fmt.Errorf("host runner must not be nil")
	}
	if !vgNamePattern.MatchString(cfg.VolumeGroup) {
		return nil, fmt.Errorf("invalid volume group name %q", cfg.VolumeGroup)
	}
	if !vgNamePattern.MatchString(cfg.VolumeGroupTag) {
		return nil, fmt.Errorf("invalid volumebgroup tag %q", cfg.VolumeGroupTag)
	}
	return &Preparer{cfg: cfg, host: host}, nil
}

// Prepare drives the node through the supported state transitions until the
// configured volume group is available on the resource disk, or fails
// closed without writing anything on unrecognized states.
func (p *Preparer) Prepare(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("node-prep")
	release, err := acquireLock(ctx, p.cfg.LockFile)

	if err != nil {
		return fmt.Errorf("failed to acquire host-wide preparation lock: %w", err)
	}
	defer release()

	if err := p.checkHostCommands(ctx); err != nil {
		return err
	}

	if err := p.waitCloudInit(ctx); err != nil {
		return err
	}

	device, err := p.resolveLink(ctx, p.cfg.ResourceDiskLink)
	if err != nil {
		if p.cfg.Required {
			return fmt.Errorf("azure resource disk link %s could not be resolved and preparation is required: %w", p.cfg.ResourceDiskLink, err)
		}
		logger.Info("azure resource disk link not found, skipping preparation; node is treated as having no resource disk", "link", p.cfg.ResourceDiskLink)
		return nil
	}
	logger.Info("resolved azure resource disk", "link", p.cfg.ResourceDiskLink, "device", device)

	disk, err := p.deviceTree(ctx, device)
	if err != nil {
		return err
	}

	if disk.Type != diskType {
		return fmt.Errorf("resource disk link %s resolves to %s of type %q, expected a whole disk", p.cfg.ResourceDiskLink, device, disk.Type)
	}

	if err := p.checkWaagentConf(); err != nil {
		return err
	}
	if err := p.checkRuntimePaths(ctx); err != nil {
		return err
	}

	pvVG, isPV, err := p.physicalVolumeVG(ctx, device)
	if err != nil {
		return err
	}
	vgDevices, vgExists, err := p.volumeGroupDevices(ctx)
	if err != nil {
		return err
	}

	switch {
	case isPV && pvVG == p.cfg.VolumeGroup:
		// Already managed: verify and activate, never wipe.
		logger.Info("resource disk already belongs to the managed volume group, verifying", "vg", p.cfg.VolumeGroup)
		return p.verifyAndActivate(ctx, device)

	case isPV && pvVG != "":
		return fmt.Errorf("resource disk %s belongs to foreign volume group %q, refusing to modify", device, pvVG)

	case isPV:
		// PV without a VG: an earlier run was interrupted between pvcreate
		// and vgcreate. Complete it.
		if vgExists {
			return fmt.Errorf("volume group %q already exists on other devices (%s) while resource disk %s is an orphaned physical volume", p.cfg.VolumeGroup, strings.Join(vgDevices, ", "), device)
		}
		logger.Info("resource disk is an orphaned physical volume from an interrupted run, creating volume group", "device", device, "vg", p.cfg.VolumeGroup)
		return p.createVolumeGroup(ctx, device)

	default:
		if vgExists {
			return fmt.Errorf("volume group %q already exists on other devices (%s), expected it on resource disk %s", p.cfg.VolumeGroup, strings.Join(vgDevices, ", "), device)
		}
	}

	state, err := p.classifyDisk(ctx, disk)
	if err != nil {
		return err
	}

	switch state {
	case diskStateBlank:
		logger.Info("resource disk is blank, creating physical volume and volume group", "device", device, "vg", p.cfg.VolumeGroup)
		if _, err := p.host.Run(ctx, "pvcreate", device); err != nil {
			return fmt.Errorf("failed to create physical volume on %s: %w", device, err)
		}
		return p.createVolumeGroup(ctx, device)

	case diskStateCloudInit:
		if !p.cfg.AllowDestructivePreparation {
			return fmt.Errorf("resource disk %s holds the cloud-init /mnt filesystem but destructive preparation is not allowed; set allowDestructivePreparation to acknowledge that all data on /mnt will be erased", device)
		}
		logger.Info("resource disk holds the recognized cloud-init /mnt layout, converting to LVM", "device", device)
		if err := p.takeOwnershipFromCloudInit(ctx, disk); err != nil {
			return err
		}
		if err := p.wipeDisk(ctx, disk); err != nil {
			return err
		}
		if _, err := p.host.Run(ctx, "pvcreate", device); err != nil {
			return fmt.Errorf("failed to create physical volume on %s: %w", device, err)
		}
		return p.createVolumeGroup(ctx, device)

	default:
		return fmt.Errorf("unrecognized resource disk state for %s, refusing to modify", device)
	}

}

type diskState int

const (
	diskStateUnknown diskState = iota
	// diskStateBlank is a disk without partitions, filesystem signatures or
	// mounts.
	diskStateBlank
	// diskStateCloudInit is exactly the recognized cloud-init layout: a
	// single ext4 resource partition, optionally mounted at /mnt, with a
	// cloud-init managed fstab entry.
	diskStateCloudInit
)

func (p *Preparer) checkHostCommands(ctx context.Context) error {
	for _, cmd := range requiredHostCommands {
		if err := p.host.LookPath(ctx, cmd); err != nil {
			return err
		}
	}
	return nil
}

func (p *Preparer) waitCloudInit(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, p.cfg.CloudInitTimeout)
	defer cancel()
	out, err := p.host.Run(waitCtx, "cloud-init", "status", "--wait")
	if err != nil {
		if waitCtx.Err() != nil {
			return fmt.Errorf("timed out after %s waiting for cloud-init to finish: %w", p.cfg.CloudInitTimeout, err)
		}
		// Newer cloud-init exits non-zero for recoverable (degraded) runs.
		// The resource-disk mount module has still completed in that case.
		if strings.Contains(string(out), "degraded") {
			return nil
		}
		return fmt.Errorf("cloud-init did not finish successfully: %w", err)
	}
	return nil
}

// checkWaagentConf verifies the Azure agent is not configured to format or
// swap-enable the resource disk, which would fight with LVM ownership. The
// agent itself stays enabled.
func (p *Preparer) checkWaagentConf() error {
	path := filepath.Join(p.cfg.HostEtcDir, "waagent.conf")
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to read %s: %w", path, err)
	}
	for line := range strings.SplitSeq(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.ToLower(strings.TrimSpace(value))
		if (key == "ResourceDisk.Format" || key == "ResourceDisk.EnableSwap") && value == "y" {
			return fmt.Errorf("waagent.conf sets %s=y; set it to n so the Azure agent does not manage the resource disk", key)
		}
	}
	return nil
}

// checkRuntimePaths rejects nodes where container-runtime or kubelet state
// resolves beneath /mnt, since erasing the resource disk would destroy it.
func (p *Preparer) checkRuntimePaths(ctx context.Context) error {
	for _, path := range hostRuntimePaths {
		out, err := p.host.Run(ctx, "readlink", "-f", path)
		if err != nil {
			return fmt.Errorf("failed to resolve host path %s: %w", path, err)
		}
		resolved := strings.TrimSpace(string(out))
		if resolved == mntTarget || strings.HasPrefix(resolved, mntTarget+"/") {
			return fmt.Errorf("host path %s resolves to %s beneath /mnt, refusing to erase the resource disk", path, resolved)
		}
	}
	return nil
}

// physicalVolumeVG returns the volume group of the device's PV, whether the
// device is a PV at all.
func (p *Preparer) physicalVolumeVG(ctx context.Context, device string) (vgName string, isPV bool, err error) {
	rows, err := p.lvmReport(ctx, "pvs", "pv", "pv_name,vg_name", "pv_name="+device)
	if err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	return rows[0]["vg_name"], true, nil
}

// volumeGroupDevices returns the PV member device paths of the configured
// volume group and whether the volume group exists.
func (p *Preparer) volumeGroupDevices(ctx context.Context) ([]string, bool, error) {
	_, exists, err := p.volumeGroupTags(ctx)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}
	rows, err := p.lvmReport(ctx, "pvs", "pv", "pv_name,vg_name", "vg_name="+p.cfg.VolumeGroup)
	if err != nil {
		return nil, false, err
	}
	devices := make([]string, 0, len(rows))
	for _, row := range rows {
		devices = append(devices, row["pv_name"])
	}
	return devices, true, nil
}

// volumeGroupTags returns the tags of the configured volume group and
// whether it exists.
func (p *Preparer) volumeGroupTags(ctx context.Context) (string, bool, error) {
	rows, err := p.lvmReport(ctx, "vgs", "vg", "vg_name,vg_tags", "vg_name="+p.cfg.VolumeGroup)
	if err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	return rows[0]["vg_tags"], true, nil
}

// createVolumeGroup creates the tagged volume group on the device and
// verifies it is discoverable through ordinary LVM commands.
func (p *Preparer) createVolumeGroup(ctx context.Context, device string) error {
	if _, err := p.host.Run(ctx, "vgcreate", "--addtag", p.cfg.VolumeGroupTag, p.cfg.VolumeGroup, device); err != nil {
		return fmt.Errorf("failed to create volume group %s on %s: %w", p.cfg.VolumeGroup, device, err)
	}
	return p.verifyAndActivate(ctx, device)
}

// verifyAndActivate refreshes LVM caches and verifies the volume group
// exists, carries the ownership tag, includes the resource disk, and is
// active.
func (p *Preparer) verifyAndActivate(ctx context.Context, device string) error {
	logger := log.FromContext(ctx).WithName("node-prep")

	if _, err := p.host.Run(ctx, "pvscan", "--cache"); err != nil {
		return fmt.Errorf("failed to refresh physical volume cache: %w", err)
	}
	if _, err := p.host.Run(ctx, "vgscan"); err != nil {
		return fmt.Errorf("failed to rescan volume groups: %w", err)
	}

	tags, exists, err := p.volumeGroupTags(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("volume group %s is not visible through ordinary LVM commands", p.cfg.VolumeGroup)
	}
	if !hasTag(tags, p.cfg.VolumeGroupTag) {
		return fmt.Errorf("volume group %s does not carry the %s ownership tag (tags: %q)", p.cfg.VolumeGroup, p.cfg.VolumeGroupTag, tags)
	}

	devices, _, err := p.volumeGroupDevices(ctx)
	if err != nil {
		return err
	}
	found := slices.Contains(devices, device)
	if !found {
		return fmt.Errorf("volume group %s exists but does not include the resource disk %s (members: %s)", p.cfg.VolumeGroup, device, strings.Join(devices, ", "))
	}

	if _, err := p.host.Run(ctx, "vgchange", "--activate", "y", p.cfg.VolumeGroup); err != nil {
		return fmt.Errorf("failed to activate volume group %s: %w", p.cfg.VolumeGroup, err)
	}

	if err := p.ensureDevicesFileEntry(ctx, device); err != nil {
		return err
	}

	logger.Info("volume group verified", "vg", p.cfg.VolumeGroup, "device", device)
	return nil
}

// ensureDevicesFileEntry registers the disk in the host's LVM devices file
// when the host uses one. pvcreate/vgcreate normally register their targets
// automatically; this covers hosts where the entry is missing.
func (p *Preparer) ensureDevicesFileEntry(ctx context.Context, device string) error {
	systemDevices := filepath.Join(p.cfg.HostEtcDir, "lvm", "devices", "system.devices")
	if _, err := os.Stat(systemDevices); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to check %s: %w", systemDevices, err)
	}
	if err := p.host.LookPath(ctx, "lvmdevices"); err != nil {
		return err
	}
	out, err := p.host.Run(ctx, "lvmdevices")
	if err != nil {
		return fmt.Errorf("failed to list LVM devices file entries: %w", err)
	}
	if strings.Contains(string(out), device) {
		return nil
	}
	if _, err := p.host.Run(ctx, "lvmdevices", "--adddev", device); err != nil {
		return fmt.Errorf("failed to register %s in the LVM devices file: %w", device, err)
	}
	return nil
}

// hasTag reports whether the comma-separated LVM tag list contains tag.
func hasTag(tags, tag string) bool {
	for t := range strings.SplitSeq(tags, ",") {
		if strings.TrimSpace(t) == tag {
			return true
		}
	}
	return false
}

// classifyDisk determines whether the (non-PV) disk is blank, holds exactly
// the recognized cloud-init layout, or must be left alone.
func (p *Preparer) classifyDisk(ctx context.Context, disk *Device) (diskState, error) {
	if holders := disk.foreignHolders(); len(holders) > 0 {
		return diskStateUnknown, fmt.Errorf("resource disk %s has foreign holders (%s), refusing to modify", disk.Path, strings.Join(holders, ", "))
	}

	parts := disk.partitions()
	for _, m := range disk.mounts() {
		if m.Target == swapMountpoint {
			return diskStateUnknown, fmt.Errorf("device %s on resource disk %s is in use as swap, refusing to modify", m.Device, disk.Path)
		}
		if m.Target != mntTarget {
			return diskStateUnknown, fmt.Errorf("device %s on resource disk %s is mounted at %s, refusing to modify", m.Device, disk.Path, m.Target)
		}
		if len(parts) != 1 || m.Device != parts[0].Path {
			return diskStateUnknown, fmt.Errorf("device %s on resource disk %s is mounted at %s but is not the single resource partition, refusing to modify", m.Device, disk.Path, mntTarget)
		}
	}

	if len(parts) == 0 {
		if disk.FSType != "" {
			return diskStateUnknown, fmt.Errorf("resource disk %s has an unrecognized filesystem signature %q, refusing to modify", disk.Path, disk.FSType)
		}
		return diskStateBlank, nil
	}

	if len(parts) != 1 || len(disk.Children) != len(parts) {
		return diskStateUnknown, fmt.Errorf("resource disk %s has an unrecognized partition layout, refusing to modify", disk.Path)
	}
	if parts[0].FSType != resourceFilesystem {
		return diskStateUnknown, fmt.Errorf("resource disk partition %s has filesystem %q, expected the cloud-init %s layout, refusing to modify", parts[0].Path, parts[0].FSType, resourceFilesystem)
	}

	// The single ext4 partition must be owned by cloud-init: its fstab
	// entry must carry the cloud-init markers and resolve to this
	// partition.
	entry, err := p.recognizedMntEntry(ctx, parts[0].Path)
	if err != nil {
		return diskStateUnknown, err
	}
	if entry == nil {
		return diskStateUnknown, fmt.Errorf("resource disk partition %s has an %s filesystem but no cloud-init fstab entry for /mnt; an arbitrary filesystem is not treated as disposable", parts[0].Path, resourceFilesystem)
	}
	return diskStateCloudInit, nil
}

// recognizedMntEntry returns the cloud-init managed /mnt fstab entry whose
// source resolves to the given partition, nil when no /mnt entry exists,
// and an error for custom or ambiguous configurations.
func (p *Preparer) recognizedMntEntry(ctx context.Context, partition string) (*fstabEntry, error) {
	content, err := p.readFstab()
	if err != nil {
		return nil, err
	}
	entry, err := findMntEntry(content)
	if err != nil || entry == nil {
		return nil, err
	}
	if !strings.HasPrefix(entry.Spec, "/") {
		return nil, fmt.Errorf("fstab entry for /mnt uses source %q which is not a recognized cloud-init device path, refusing to modify", entry.Spec)
	}
	resolved, err := p.resolveLink(ctx, entry.Spec)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve fstab /mnt source %s: %w", entry.Spec, err)
	}
	if resolved != partition {
		return nil, fmt.Errorf("fstab entry for /mnt resolves to %s, not the resource partition %s, refusing to modify", resolved, partition)
	}
	return entry, nil
}

func (p *Preparer) fstabPath() string {
	return filepath.Join(p.cfg.HostEtcDir, "fstab")
}

func (p *Preparer) readFstab() (string, error) {
	content, err := os.ReadFile(p.fstabPath())
	if err != nil {
		return "", fmt.Errorf("failed to read host fstab: %w", err)
	}
	return string(content), nil
}

// takeOwnershipFromCloudInit unmounts /mnt and removes cloud-init's exact
// fstab ownership of the resource partition.
func (p *Preparer) takeOwnershipFromCloudInit(ctx context.Context, disk *Device) error {
	logger := log.FromContext(ctx).WithName("node-prep")
	partition := disk.partitions()[0].Path

	// Reject recursive mounts below /mnt: something else is using the
	// tree and unmounting would hide it.
	out, err := p.host.Run(ctx, "findmnt", "--list", "--noheadings", "--output", "TARGET")
	if err != nil {
		return fmt.Errorf("failed to list host mounts: %w", err)
	}
	for target := range strings.FieldsSeq(string(out)) {
		if strings.HasPrefix(target, mntTarget+"/") {
			return fmt.Errorf("found mount %s below /mnt, refusing to unmount", target)
		}
	}

	// Stop the systemd mount unit generated from fstab. A plain umount is
	// the fallback for a manually mounted partition; it fails when /mnt has
	// users, which is exactly the fail-closed behavior we want.
	if p.isMounted(ctx) {
		if _, err := p.host.Run(ctx, "systemctl", "stop", "mnt.mount"); err != nil {
			logger.V(1).Info("systemctl stop mnt.mount failed, falling back to umount", "error", err.Error())
		}
		if p.isMounted(ctx) {
			if _, err := p.host.Run(ctx, "umount", mntTarget); err != nil {
				return fmt.Errorf("failed to unmount /mnt: %w", err)
			}
		}
		if p.isMounted(ctx) {
			return fmt.Errorf("/mnt is still mounted after unmounting")
		}
	}

	// Re-verify and neutralize the fstab entry after the unmount so a
	// concurrent change cannot slip in between validation and edit.
	content, err := p.readFstab()
	if err != nil {
		return err
	}
	entry, err := findMntEntry(content)
	if err != nil {
		return err
	}
	if entry != nil {
		resolved, err := p.resolveLink(ctx, entry.Spec)
		if err != nil {
			return fmt.Errorf("failed to resolve fstab /mnt source %s: %w", entry.Spec, err)
		}
		if resolved != partition {
			return fmt.Errorf("fstab entry for /mnt resolves to %s, not the resource partition %s, refusing to modify", resolved, partition)
		}
		backupPath := p.fstabPath() + fstabBackupSuffix
		if err := writeFileAtomic(backupPath, []byte(content), 0600); err != nil {
			return fmt.Errorf("failed to back up fstab: %w", err)
		}
		logger.Info("backed up host fstab", "backup", backupPath)

		updated, err := neutralizeFstabLine(content, entry.LineNo)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(p.fstabPath(), []byte(updated), 0644); err != nil {
			return fmt.Errorf("failed to update fstab: %w", err)
		}
		logger.Info("neutralized cloud-init /mnt entry in host fstab", "source", entry.Spec)

		if _, err := p.host.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("failed to reload systemd units after fstab change: %w", err)
		}
		if _, err := p.host.Run(ctx, "findmnt", "--verify"); err != nil {
			return fmt.Errorf("fstab verification failed after modification: %w", err)
		}
	}

	if p.isMounted(ctx) {
		return fmt.Errorf("/mnt is still mounted after removing its fstab entry")
	}
	return nil
}

// isMounted reports whether /mnt is an active mount point on the host.
func (p *Preparer) isMounted(ctx context.Context) bool {
	_, err := p.host.Run(ctx, "findmnt", "--mountpoint", mntTarget)
	return err == nil
}

// wipeDisk removes the partition table and all signatures from the resource
// disk. The stable link identity is re-checked immediately before the first
// destructive write.
func (p *Preparer) wipeDisk(ctx context.Context, disk *Device) error {
	logger := log.FromContext(ctx).WithName("node-prep")

	// Re-resolve the stable link and confirm the device identity has not
	// changed between preflight and wipe.
	device, err := p.resolveLink(ctx, p.cfg.ResourceDiskLink)
	if err != nil {
		return fmt.Errorf("failed to re-resolve resource disk link before wiping: %w", err)
	}
	current, err := p.deviceTree(ctx, device)
	if err != nil {
		return err
	}
	if device != disk.Path || current.MajMin != disk.MajMin {
		return fmt.Errorf("resource disk identity changed between preflight (%s %s) and wipe (%s %s), aborting", disk.Path, disk.MajMin, device, current.MajMin)
	}

	// Wipe the actual enumerated partitions, then the whole disk. Device
	// paths come from lsblk; nothing is constructed by name.
	for _, part := range current.partitions() {
		logger.Info("wiping signatures on resource disk partition", "partition", part.Path)
		if _, err := p.host.Run(ctx, "wipefs", "--all", part.Path); err != nil {
			return fmt.Errorf("failed to wipe partition %s: %w", part.Path, err)
		}
	}
	logger.Info("wiping partition table on resource disk", "device", device)
	if _, err := p.host.Run(ctx, "wipefs", "--all", "--force", device); err != nil {
		return fmt.Errorf("failed to wipe disk %s: %w", device, err)
	}

	if _, err := p.host.Run(ctx, "blockdev", "--rereadpt", device); err != nil {
		return fmt.Errorf("failed to reread partition table on %s: %w", device, err)
	}
	if _, err := p.host.Run(ctx, "udevadm", "settle"); err != nil {
		return fmt.Errorf("failed to settle udev after wiping %s: %w", device, err)
	}

	// Verify nothing remains.
	after, err := p.deviceTree(ctx, device)
	if err != nil {
		return err
	}
	if len(after.Children) > 0 || after.FSType != "" {
		return fmt.Errorf("resource disk %s still has children or signatures after wiping", device)
	}
	return nil
}
