// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lvm

import (
	"strings"

	"local-csi-driver/internal/pkg/probe"
)

// StorageClass parameters for customizing disk selection. Each accepts a
// comma-separated list of values. A disk is selected only if it matches all
// three parameters; within a parameter, any value may match. Unspecified
// parameters fall back to the defaults in the probe package.
var (
	// DiskPathPrefixesParam selects disks by device path prefix, e.g.
	// "/dev/nvme".
	DiskPathPrefixesParam = DriverName + "/disk-path-prefixes"

	// DiskModelsParam selects disks by model, e.g.
	// "Microsoft NVMe Direct Disk".
	DiskModelsParam = DriverName + "/disk-models"

	// DiskTypesParam selects disks by device type, e.g. "disk".
	DiskTypesParam = DriverName + "/disk-types"
)

// DiskSelectionDefaults holds driver-level default disk selection values,
// typically parsed from driver flags. Empty slices fall back to the probe
// package defaults. Like the built-in defaults, these are intentionally
// never persisted in the volume context so they can change between driver
// deployments without breaking existing persistent volumes.
type DiskSelectionDefaults struct {
	PathPrefixes []string
	Models       []string
	Types        []string
}

// NewDiskSelectionDefaults parses comma-separated flag values into disk
// selection defaults. Empty values keep the built-in defaults.
func NewDiskSelectionDefaults(pathPrefixes, models, types string) DiskSelectionDefaults {
	return DiskSelectionDefaults{
		PathPrefixes: splitParam(pathPrefixes),
		Models:       splitParam(models),
		Types:        splitParam(types),
	}
}

// Filter returns the disk filter for these defaults, used when no
// per-volume parameters are available (e.g. the startup diagnostic).
func (d DiskSelectionDefaults) Filter() *probe.Filter {
	return probe.NewDiskFilter(d.PathPrefixes, d.Models, d.Types)
}

// diskFilterFromParams builds a disk filter from StorageClass parameters or
// PV volume attributes. Each absent or empty parameter falls back to the
// driver-level default, and then to the probe package default. Defaults are
// intentionally not persisted in the volume context so they can change
// between driver versions.
func (l *LVM) diskFilterFromParams(params map[string]string) *probe.Filter {
	return probe.NewDiskFilter(
		paramOrDefault(params[DiskPathPrefixesParam], l.diskDefaults.PathPrefixes),
		paramOrDefault(params[DiskModelsParam], l.diskDefaults.Models),
		paramOrDefault(params[DiskTypesParam], l.diskDefaults.Types),
	)
}

// paramOrDefault splits a parameter value, falling back to the given
// default when the value has no entries.
func paramOrDefault(value string, def []string) []string {
	if v := splitParam(value); v != nil {
		return v
	}
	return def
}

// splitParam splits a comma-separated parameter value, trimming whitespace
// and dropping empty entries. It returns nil if no entries remain, so the
// caller treats the parameter as unspecified.
func splitParam(value string) []string {
	var entries []string
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}
