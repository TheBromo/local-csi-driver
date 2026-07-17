package nodeprep

import (
	"regexp"
	"time"
)

const (
	// DefaultResourceDiskLink is the stable udev link Azure maintains for
	// the ephemeral resource disk. Azure documents this as the predictable
	// way to address the resource disk instead of kernel-assigned names
	// like /dev/sdb.
	DefaultResourceDiskLink = "/dev/disk/azure/resource"

	// DefaultVolumeGroupTag marks volume groups managed by this driver. It
	// must match the tag used by the CSI driver's LVM core.
	DefaultVolumeGroupTag = "local-csi"

	// DefaultCloudInitTimeout bounds the wait for cloud-init to finish.
	DefaultCloudInitTimeout = 10 * time.Minute

	// DefaultHostEtcDir is where the host's /etc is mounted inside the
	// init container.
	DefaultHostEtcDir = "/host/etc"

	// DefaultLockFile is the host-wide lock serializing node preparation.
	// It lives on the host's /run tmpfs so stale locks disappear on reboot.
	DefaultLockFile = "/host/run/local-csi-driver-node-prep.lock"

	// fstabBackupSuffix is appended to the fstab path for the backup taken
	// before modifying it.
	fstabBackupSuffix = ".local-csi-driver-node-prep.bak"

	// resourceFilesystem is the filesystem cloud-init/waagent create on the
	// Azure resource-disk partition. Only this layout is recognized as
	// disposable.
	resourceFilesystem = "ext4"

	// mntTarget is the mount point cloud-init assigns to the Azure resource
	// disk. It is the only candidate-backed mount preparation may remove;
	// any other mount (/, /boot, kubelet, containerd, pod mounts, ...)
	// fails preflight.
	mntTarget = "/mnt"
)

// hostRuntimePaths must not resolve beneath /mnt, otherwise erasing the
// resource disk would take down the container runtime or kubelet.
var hostRuntimePaths = []string{
	"/var/lib/kubelet",
	"/var/lib/containerd",
	"/var/lib/docker",
}

var vgNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+_.-]*$`)

// requiredHostCommands must all be available on the host before any
// preparation step runs.
var requiredHostCommands = []string{
	"cloud-init",
	"readlink",
	"lsblk",
	"udevadm",
	"wipefs",
	"blockdev",
	"systemctl",
	"findmnt",
	"umount",
	"pvs",
	"vgs",
	"pvcreate",
	"vgcreate",
	"vgchange",
	"pvscan",
	"vgscan",
}

// Config configures node preparation.
type Config struct {
	// VolumeGroup is the LVM volume group to create or verify on the
	// resource disk.
	VolumeGroup string
	// VolumeGroupTag is the ownership tag added to the volume group.
	VolumeGroupTag string
	// ResourceDiskLink is the stable link to the Azure resource disk.
	ResourceDiskLink string
	// Required fails preparation when the resource disk link is missing.
	// When false, a missing link is logged and treated as a node without
	// this disk class.
	Required bool
	// AllowDestructivePreparation acknowledges that the cloud-init /mnt
	// filesystem on the resource disk will be erased. Without it, any state
	// requiring destructive changes fails before writes.
	AllowDestructivePreparation bool
	// CloudInitTimeout bounds the cloud-init status --wait call.
	CloudInitTimeout time.Duration
	// HostEtcDir is the mount point of the host's /etc inside the
	// container.
	HostEtcDir string
	// LockFile is the host-wide lock file path (on a shared host mount).
	LockFile string
}

func (c Config) withDefaults() Config {
	if c.VolumeGroupTag == "" {
		c.VolumeGroupTag = DefaultVolumeGroupTag
	}
	if c.ResourceDiskLink == "" {
		c.ResourceDiskLink = DefaultResourceDiskLink
	}
	if c.CloudInitTimeout <= 0 {
		c.CloudInitTimeout = DefaultCloudInitTimeout
	}
	if c.HostEtcDir == "" {
		c.HostEtcDir = DefaultHostEtcDir
	}
	if c.LockFile == "" {
		c.LockFile = DefaultLockFile
	}
	return c
}
