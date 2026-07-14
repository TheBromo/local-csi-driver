// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package block

// Device represents a block device in the lsblk output.
type Device struct {
	Name        string   `json:"name"`
	Path        string   `json:"path,omitempty"`
	MajMin      string   `json:"maj:min,omitempty"`
	Removable   bool     `json:"rm,omitempty"`
	Type        string   `json:"type,omitempty"`
	Mountpoint  string   `json:"mountpoint,omitempty"`
	Mountpoints []string `json:"mountpoints,omitempty"`
	Model       string   `json:"model,omitempty"`
	Serial      string   `json:"serial,omitempty"`
	Size        int64    `json:"size,omitempty"`
	Children    []Device `json:"children,omitempty"`
	Adopted     bool     `json:"adopted,omitempty"`
}

// DeviceList represents the output of the lsblk command.
type DeviceList struct {
	Devices []Device `json:"blockdevices"`
}

// MountedPaths returns all non-empty mountpoints on this device and its
// children. Child mountpoints are returned before parent mountpoints so callers
// can unmount nested devices before wiping their parents.
func (d Device) MountedPaths() []string {
	mounts := make([]string, 0, len(d.Mountpoints)+len(d.Children))
	seen := map[string]struct{}{}
	appendMount := func(mountpoint string) {
		if mountpoint == "" {
			return
		}
		if _, ok := seen[mountpoint]; ok {
			return
		}
		seen[mountpoint] = struct{}{}
		mounts = append(mounts, mountpoint)
	}

	for _, child := range d.Children {
		for _, mountpoint := range child.MountedPaths() {
			appendMount(mountpoint)
		}
	}
	appendMount(d.Mountpoint)
	for _, mountpoint := range d.Mountpoints {
		appendMount(mountpoint)
	}
	return mounts
}

// DevicePaths returns this device and all child device paths. Child paths are
// returned before parent paths so filesystem signatures are wiped before the
// partition table that exposes those children.
func (d Device) DevicePaths() []string {
	paths := make([]string, 0, 1+len(d.Children))
	for _, child := range d.Children {
		paths = append(paths, child.DevicePaths()...)
	}
	if d.Path != "" {
		paths = append(paths, d.Path)
	}
	return paths
}
