// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// diskType is the lsblk type for a whole disk.
	diskType = "disk"
	// partType is the lsblk type for a partition.
	partType = "part"
	// swapMountpoint is the pseudo mountpoint lsblk reports for active swap.
	swapMountpoint = "[SWAP]"
)

// Device is a node in the lsblk device tree of the candidate disk.
type Device struct {
	Path        string   `json:"path"`
	KName       string   `json:"kname"`
	Type        string   `json:"type"`
	FSType      string   `json:"fstype"`
	MajMin      string   `json:"maj:min"`
	Mountpoint  string   `json:"mountpoint"`
	Mountpoints []string `json:"mountpoints"`
	Children    []Device `json:"children,omitempty"`
}

// deviceTree is the top-level lsblk JSON structure.
type deviceTree struct {
	BlockDevices []Device `json:"blockdevices"`
}

// MountUse records that a device in the candidate disk's tree backs a
// mount target.
type MountUse struct {
	Device string
	Target string
}

// parseLsblkTree parses lsblk --json output for a single device and returns
// its tree.
func parseLsblkTree(out []byte) (*Device, error) {
	var tree deviceTree
	if err := json.Unmarshal(out, &tree); err != nil {
		return nil, fmt.Errorf("failed to parse lsblk output: %w", err)
	}
	if len(tree.BlockDevices) != 1 {
		return nil, fmt.Errorf("expected exactly one block device in lsblk output, got %d", len(tree.BlockDevices))
	}
	return &tree.BlockDevices[0], nil
}

// walk visits the device and all its descendants.
func (d *Device) walk(fn func(*Device)) {
	fn(d)
	for i := range d.Children {
		d.Children[i].walk(fn)
	}
}

// mounts returns every mount target backed by the device or any of its
// descendants, including active swap (reported as "[SWAP]").
func (d *Device) mounts() []MountUse {
	var uses []MountUse
	d.walk(func(dev *Device) {
		seen := map[string]struct{}{}
		add := func(target string) {
			if target == "" {
				return
			}
			if _, ok := seen[target]; ok {
				return
			}
			seen[target] = struct{}{}
			uses = append(uses, MountUse{Device: dev.Path, Target: target})
		}
		add(dev.Mountpoint)
		for _, m := range dev.Mountpoints {
			add(m)
		}
	})
	return uses
}

// partitions returns the direct partition children of the disk.
func (d *Device) partitions() []Device {
	parts := make([]Device, 0, len(d.Children))
	for _, c := range d.Children {
		if c.Type == partType {
			parts = append(parts, c)
		}
	}
	return parts
}

// foreignHolders returns descendants that are neither the disk itself nor
// plain partitions: device-mapper, MD RAID, crypt, LVM or any other stacked
// consumer of the disk.
func (d *Device) foreignHolders() []string {
	var holders []string
	d.walk(func(dev *Device) {
		if dev == d {
			return
		}
		if dev.Type != partType {
			holders = append(holders, fmt.Sprintf("%s (%s)", dev.Path, dev.Type))
		}
	})
	return holders
}

// resolveLink resolves a symlink on the host to its canonical target. It
// fails if the target does not exist.
func (p *Preparer) resolveLink(ctx context.Context, link string) (string, error) {
	out, err := p.host.Run(ctx, "readlink", "-e", link)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(out))
	if target == "" {
		return "", fmt.Errorf("link %s resolved to an empty path", link)
	}
	return target, nil
}

// deviceTree fetches the lsblk tree for the device from the host.
func (p *Preparer) deviceTree(ctx context.Context, device string) (*Device, error) {
	out, err := p.host.Run(ctx, "lsblk", "--json", "--bytes", "--paths",
		"--output", "PATH,KNAME,TYPE,FSTYPE,MAJ:MIN,MOUNTPOINT,MOUNTPOINTS", device)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect device %s: %w", device, err)
	}
	return parseLsblkTree(out)
}
