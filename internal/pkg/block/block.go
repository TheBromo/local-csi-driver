// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package block

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"syscall"

	utilexec "k8s.io/utils/exec"
)

const (
	// lsblkCommand is the command to list block devices.
	lsblkCommand = "lsblk"
	blkidCmd     = "blkid"
	nsenterCmd   = "nsenter"
	umountCmd    = "umount"
	wipefsCmd    = "wipefs"
	blockdevCmd  = "blockdev"
	Lvm2Type     = "LVM2_member"

	partitionRereadBusyOutput = "BLKRRPART: Device or resource busy"
)

var (
	blkidTypeRe = regexp.MustCompile(`\bTYPE="([^"]+)"`)

	// ErrDeviceMounted is returned when adopting a formatted device would
	// require unmounting but the caller did not explicitly allow it.
	ErrDeviceMounted = errors.New("device is mounted")
)

// Interface defines the methods that block should implement
//
//go:generate mockgen -copyright_file ../../../hack/mockgen_copyright.txt -destination=mock_block.go -mock_names=Interface=Mock -package=block -source=block.go Interface
type Interface interface {
	GetDevice(ctx context.Context, path string) (*Device, error)
	GetDevices(ctx context.Context) (*DeviceList, error)
	AdoptDevice(ctx context.Context, device Device, opts AdoptDeviceOptions) error
	IsBlockDevice(path string) (bool, error)
	IsFormatted(device string) (bool, error)
	IsLVM2(device string) (bool, error)
}

// AdoptDeviceOptions controls how a formatted block device may be prepared for
// LVM use.
type AdoptDeviceOptions struct {
	AllowMounted bool
}

// block implements the Interface.
type block struct {
	exec utilexec.Interface
}

var _ Interface = &block{}

// New returns a new block instance.
func New() Interface {
	return &block{
		exec: utilexec.New(),
	}
}

// GetDevices runs the lsblk --json command and parses the output.
func (l *block) GetDevices(ctx context.Context) (*DeviceList, error) {
	_, err := l.exec.LookPath(lsblkCommand)
	if err != nil {
		return nil, fmt.Errorf("unable to find %s in PATH: %w", lsblkCommand, err)
	}

	cmd := l.exec.CommandContext(ctx, lsblkCommand, "--bytes", "--json", "--output-all")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("command failed: %w, output: %s", err, string(output))
	}

	return parseLsblkOutput(output)
}

func (l *block) GetDevice(ctx context.Context, path string) (*Device, error) {
	if path == "" {
		return nil, fmt.Errorf("device path is required")
	}

	_, err := l.exec.LookPath(lsblkCommand)
	if err != nil {
		return nil, fmt.Errorf("unable to find %s in PATH: %w", lsblkCommand, err)
	}

	cmd := l.exec.CommandContext(ctx, lsblkCommand, "--bytes", "--json", "--output-all", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("command failed: %w, output: %s", err, string(output))
	}

	devices, err := parseLsblkOutput(output)
	if err != nil {
		return nil, err
	}
	if len(devices.Devices) != 1 {
		return nil, fmt.Errorf("expected one device for %s, got %d", path, len(devices.Devices))
	}
	return &devices.Devices[0], nil
}

// AdoptDevice removes existing mount and filesystem metadata from a block
// device so it can be initialized as an LVM physical volume.
func (l *block) AdoptDevice(ctx context.Context, device Device, opts AdoptDeviceOptions) error {
	refreshed, err := l.GetDevice(ctx, device.Path)
	if err != nil {
		return fmt.Errorf("failed to refresh device tree for %s: %w", device.Path, err)
	}
	device = *refreshed

	mounts := device.MountedPaths()
	if len(mounts) > 0 && !opts.AllowMounted {
		return fmt.Errorf("%w: %s", ErrDeviceMounted, device.Path)
	}

	for _, mountpoint := range mounts {
		cmd := l.exec.CommandContext(ctx, nsenterCmd, "--target", "1", "--mount", "--", umountCmd, mountpoint)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to unmount %s for device %s: %w, output: %s", mountpoint, device.Path, err, string(output))
		}
	}

	for _, path := range device.DevicePaths() {
		cmd := l.exec.CommandContext(ctx, wipefsCmd, "--all", "--force", path)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to wipe signatures from %s: %w, output: %s", path, err, string(output))
		}
	}

	if device.Path != "" {
		cmd := l.exec.CommandContext(ctx, blockdevCmd, "--rereadpt", device.Path)
		if output, err := cmd.CombinedOutput(); err != nil {
			if isPartitionRereadBusy(output, err) {
				return nil
			}
			return fmt.Errorf("failed to reread partition table for %s: %w, output: %s", device.Path, err, string(output))
		}
	}
	return nil
}

func isPartitionRereadBusy(output []byte, err error) bool {
	exit, ok := err.(utilexec.ExitError)
	return ok && exit.ExitStatus() == 1 && strings.Contains(string(output), partitionRereadBusyOutput)
}

// IsBlockDevice reports whether the given path is a block device.
func (s *block) IsBlockDevice(path string) (bool, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("failed to stat path %s: %w", path, err)
	}

	stat, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("failed to get raw syscall.Stat_t data for %s", path)
	}

	// Check if the file mode is a block device
	if (stat.Mode & syscall.S_IFMT) == syscall.S_IFBLK {
		return true, nil
	}

	return false, nil
}

// IsFormatted reports whether the given block device is formatted.
func (s *block) IsFormatted(blockDev string) (bool, error) {
	_, isFormatted, err := s.blkid(blockDev)
	if err != nil {
		return false, err
	}
	return isFormatted, nil
}

// IsLVM2 reports whether the given block device is an LVM2 physical volume.
func (s *block) IsLVM2(blockDev string) (bool, error) {
	fsType, isFormatted, err := s.blkid(blockDev)
	if err != nil {
		return false, err
	}
	if !isFormatted {
		return false, nil
	}
	return fsType == Lvm2Type, nil
}

// blkid reports whether the given block device is formatted and returns its filesystem type if present.
// blockDev is the path to the block device e.g. /dev/nvme0n1.
// Uses system utility blkid to get information about the device.
// blkid exit status is:
//   - 0, the device is present and responds to information i.e. it has a filesystem.
//   - 2, device Not present or does not have a filesystem.
//   - 4, usage or other errors.
//   - 8, ambivalent probing result was detected by low-level probing mode (-p).
//
// Returns: fsType (string), formatted (bool), error.
func (s *block) blkid(blockDev string) (string, bool, error) {
	devPresent, err := s.IsBlockDevice(blockDev)
	if err != nil {
		return "", false, err
	}
	if !devPresent {
		return "", false, fmt.Errorf("device %s not present", blockDev)
	}

	args := []string{"-p", blockDev}
	out, err := s.exec.Command(blkidCmd, args...).CombinedOutput()
	if err == nil {
		// Exit code 0: device is formatted
		fsType := parseFsTypeFromBlkid(string(out))
		return fsType, true, nil
	}
	if exit, ok := err.(utilexec.ExitError); ok {
		if exit.ExitStatus() == 2 {
			// Exit code 2: device is unformatted
			return "", false, nil
		}

	}
	return "", false, fmt.Errorf("could not determine if device %s is formatted: %w", blockDev, err)
}

// parseFsTypeFromBlkid extracts TYPE="<fstype>" from blkid output.
// It ignores PTTYPE and other attributes.
func parseFsTypeFromBlkid(out string) string {
	m := blkidTypeRe.FindStringSubmatch(out)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// parseLsblkOutput parses the JSON output of the lsblk command.
func parseLsblkOutput(output []byte) (*DeviceList, error) {
	var lsblkOutput DeviceList
	err := json.Unmarshal(output, &lsblkOutput)
	if err != nil {
		return nil, fmt.Errorf("failed to parse lsblk output: %w", err)
	}
	return &lsblkOutput, nil
}
