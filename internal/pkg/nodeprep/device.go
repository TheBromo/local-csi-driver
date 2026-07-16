// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// DeviceType is the lsblk TYPE value for a block device.
type DeviceType string

const (
	deviceTypeDisk  DeviceType = "disk"
	deviceTypePart  DeviceType = "part"
	deviceTypeLVM   DeviceType = "lvm"
	deviceTypeCrypt DeviceType = "crypt"
	deviceTypeDM    DeviceType = "dm"
	deviceTypeMD    DeviceType = "md"
)

// BlockDevice is the subset of lsblk data used for preparation decisions.
type BlockDevice struct {
	Name           string          `json:"name"`
	Path           string          `json:"path"`
	KernelName     string          `json:"kname"`
	ParentName     string          `json:"pkname"`
	MajorMinor     string          `json:"maj:min"`
	Type           DeviceType      `json:"type"`
	Filesystem     string          `json:"fstype"`
	PartitionTable string          `json:"pttype"`
	Mountpoints    nullableStrings `json:"mountpoints"`
	Children       []BlockDevice   `json:"children"`
}

// DeviceGraph contains the resource disk's child and holder trees.
type DeviceGraph struct {
	Disk       BlockDevice
	Candidates map[string]BlockDevice
	Holders    []BlockDevice
}

type lsblkReport struct {
	Devices []BlockDevice `json:"blockdevices"`
}

type nullableStrings []string

func (s *nullableStrings) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*s = nil
		return nil
	}

	var values []*string
	if err := json.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("parse nullable string list: %w", err)
	}

	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != nil && *value != "" {
			result = append(result, *value)
		}
	}
	*s = result
	return nil
}

func parseDeviceGraph(childrenOutput, holdersOutput []byte, expectedIdentity string) (*DeviceGraph, error) {
	children, err := parseLSBLK(childrenOutput)
	if err != nil {
		return nil, fmt.Errorf("parse lsblk child graph: %w", err)
	}

	disk, found := findDeviceByIdentity(children.Devices, expectedIdentity)
	if !found {
		return nil, fmt.Errorf("resource disk identity %s not present in lsblk output", expectedIdentity)
	}
	if disk.Type != deviceTypeDisk {
		return nil, fmt.Errorf("resource link resolved to %s device %s, expected a whole disk", disk.Type, disk.Path)
	}

	holders, err := parseLSBLK(holdersOutput)
	if err != nil {
		return nil, fmt.Errorf("parse lsblk holder graph: %w", err)
	}
	holderRoot, found := findDeviceByIdentity(holders.Devices, expectedIdentity)
	if !found {
		return nil, fmt.Errorf("resource disk identity %s not present in inverse lsblk output", expectedIdentity)
	}

	candidates := make(map[string]BlockDevice)
	collectDevices(disk, candidates)

	return &DeviceGraph{
		Disk:       disk,
		Candidates: candidates,
		Holders:    flattenDevices(holderRoot.Children),
	}, nil
}

func parseSingleDevice(output []byte) (BlockDevice, error) {
	report, err := parseLSBLK(output)
	if err != nil {
		return BlockDevice{}, err
	}
	if len(report.Devices) != 1 {
		return BlockDevice{}, fmt.Errorf("expected one block device, found %d", len(report.Devices))
	}
	return report.Devices[0], nil
}

func parseLSBLK(output []byte) (*lsblkReport, error) {
	var report lsblkReport
	if err := json.Unmarshal(output, &report); err != nil {
		return nil, fmt.Errorf("decode lsblk JSON: %w", err)
	}
	if len(report.Devices) == 0 {
		return nil, fmt.Errorf("lsblk returned no block devices")
	}
	return &report, nil
}

func findDeviceByIdentity(devices []BlockDevice, identity string) (BlockDevice, bool) {
	for _, device := range devices {
		if device.MajorMinor == identity {
			return device, true
		}
		if found, ok := findDeviceByIdentity(device.Children, identity); ok {
			return found, true
		}
	}
	return BlockDevice{}, false
}

func collectDevices(device BlockDevice, result map[string]BlockDevice) {
	result[device.MajorMinor] = device
	for _, child := range device.Children {
		collectDevices(child, result)
	}
}

func flattenDevices(devices []BlockDevice) []BlockDevice {
	result := make([]BlockDevice, 0, len(devices))
	for _, device := range devices {
		result = append(result, device)
		result = append(result, flattenDevices(device.Children)...)
	}
	return result
}

func (g *DeviceGraph) directPartitions() []BlockDevice {
	partitions := make([]BlockDevice, 0, len(g.Disk.Children))
	for _, child := range g.Disk.Children {
		if child.Type == deviceTypePart {
			partitions = append(partitions, child)
		}
	}
	return partitions
}

func (g *DeviceGraph) isBlank() bool {
	return g.Disk.Filesystem == "" && g.Disk.PartitionTable == "" && len(g.Disk.Children) == 0 && len(g.Holders) == 0
}

func (g *DeviceGraph) hasIdentity(identity string) bool {
	if _, ok := g.Candidates[identity]; ok {
		return true
	}
	for _, holder := range g.Holders {
		if holder.MajorMinor == identity {
			return true
		}
	}
	return false

}

func (g *DeviceGraph) containsPath(path string) bool {
	for _, device := range g.Candidates {
		if filepath.Clean(device.Path) == filepath.Clean(path) {
			return true
		}
	}
	for _, holder := range g.Holders {
		if filepath.Clean(holder.Path) == filepath.Clean(path) {
			return true
		}
	}
	return false
}

func (g *DeviceGraph) rejectForeignHolders(allowLVM bool) error {
	for _, holder := range g.Holders {
		if allowLVM && holder.Type == deviceTypeLVM {
			continue
		}
		return fmt.Errorf("resource disk has unsupported %s holder %s", holder.Type, holder.Path)
	}
	return nil
}

type Mount struct {
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	MajorMinor string  `json:"maj:min"`
	Filesystem string  `json:"fstype"`
	Options    string  `json:"options"`
	Children   []Mount `json:"children"`
}

type findmntReport struct {
	Filesystems []Mount `json:"filesystems"`
}

func parseMounts(output []byte) ([]Mount, error) {
	var report findmntReport
	if err := json.Unmarshal(output, &report); err != nil {
		return nil, fmt.Errorf("decode findmnt JSON: %w", err)
	}

	var mounts []Mount
	var flatten func([]Mount)
	flatten = func(entries []Mount) {
		for _, entry := range entries {
			mounts = append(mounts, entry)
			flatten(entry.Children)
		}
	}
	flatten(report.Filesystems)
	return mounts, nil
}

func candidateMounts(graph *DeviceGraph, mounts []Mount) []Mount {
	var result []Mount
	for _, mount := range mounts {
		if graph.hasIdentity(mount.MajorMinor) {
			result = append(result, mount)
		}
	}
	return result
}

func validateCandidateMounts(graph *DeviceGraph, mounts []Mount) (*BlockDevice, error) {
	candidate := candidateMounts(graph, mounts)
	if len(candidate) == 0 {
		return nil, nil
	}

	partitions := graph.directPartitions()
	if len(candidate) != 1 || len(partitions) != 1 {
		return nil, fmt.Errorf("resource disk has %d backed mounts and %d direct partitions; only one cloud-init /mnt partition is supported", len(candidate), len(partitions))
	}
	mount := candidate[0]
	partition := partitions[0]
	if filepath.Clean(mount.Target) != "/mnt" || mount.MajorMinor != partition.MajorMinor {
		return nil, fmt.Errorf("resource-backed mount %s at %s is not the recognized resource partition mounted directly at /mnt", mount.Source, mount.Target)
	}
	return &partition, nil
}

func pathWithin(path, directory string) bool {
	cleanPath := filepath.Clean(path)
	cleanDirectory := filepath.Clean(directory)
	return cleanPath == cleanDirectory || strings.HasPrefix(cleanPath, cleanDirectory+string(filepath.Separator))
}

type physicalVolume struct {
	Name        string `json:"pv_name"`
	VolumeGroup string `json:"vg_name"`
}

type volumeGroup struct {
	Name string `json:"vg_name"`
	Tags string `json:"vg_tags"`
}

type lvmReportSection struct {
	PhysicalVolumes []physicalVolume `json:"pv"`
	VolumeGroups    []volumeGroup    `json:"vg"`
}

type lvmReport struct {
	Reports []lvmReportSection `json:"report"`
}

func parsePhysicalVolumes(output []byte) ([]physicalVolume, error) {
	var report lvmReport
	if err := json.Unmarshal(output, &report); err != nil {
		return nil, fmt.Errorf("decode pvs JSON: %w", err)
	}
	if len(report.Reports) == 0 {
		return nil, nil
	}
	return report.Reports[0].PhysicalVolumes, nil
}

func parseVolumeGroups(output []byte) ([]volumeGroup, error) {
	var report lvmReport
	if err := json.Unmarshal(output, &report); err != nil {
		return nil, fmt.Errorf("decode vgs JSON: %w", err)
	}
	if len(report.Reports) == 0 {
		return nil, nil
	}
	return report.Reports[0].VolumeGroups, nil
}

func hasTag(tags, expected string) bool {
	for _, tag := range strings.Split(tags, ",") {
		if strings.TrimSpace(tag) == expected {
			return true
		}
	}
	return false
}

func sortedDevicePaths(devices []BlockDevice) []string {
	paths := make([]string, 0, len(devices))
	for _, device := range devices {
		paths = append(paths, device.Path)
	}
	sort.Strings(paths)
	return paths
}
