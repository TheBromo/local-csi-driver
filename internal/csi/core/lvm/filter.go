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

// diskFilterFromParams builds a disk filter from StorageClass parameters or
// PV volume attributes. Absent or empty parameters fall back to the probe
// package defaults. The defaults are intentionally not persisted in the
// volume context so they can change between driver versions.
func diskFilterFromParams(params map[string]string) *probe.Filter {
	return probe.NewDiskFilter(
		splitParam(params[DiskPathPrefixesParam]),
		splitParam(params[DiskModelsParam]),
		splitParam(params[DiskTypesParam]),
	)
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
