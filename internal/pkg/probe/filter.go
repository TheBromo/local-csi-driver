// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package probe

import (
	"strings"

	"local-csi-driver/internal/pkg/block"
)

// Default disk selection values used when the corresponding StorageClass
// parameter is not specified. These are kept in code, not persisted in the
// volume context, so they can be adjusted in future driver versions without
// breaking existing persistent volumes.
var (
	// DefaultDiskPathPrefixes is the default disk path prefix filter.
	DefaultDiskPathPrefixes = []string{"/dev/nvme"}

	// DefaultDiskModels is the default disk model filter.
	DefaultDiskModels = []string{"Microsoft NVMe Direct Disk", "Microsoft NVMe Direct Disk v2"}

	// DefaultDiskTypes is the default disk type filter.
	DefaultDiskTypes = []string{"disk"}
)

// Wildcard matches any value when used in a disk selection parameter.
const Wildcard = "*"

// EphemeralDiskFilter is the default filter for ephemeral disks.
var EphemeralDiskFilter = NewDiskFilter(nil, nil, nil)

// NewDiskFilter creates a filter matching devices by path prefix, model and
// type. A device must match all three predicates; within a predicate, any
// value may match. Empty or nil slices fall back to the package defaults.
// A Wildcard ("*") value, even mixed with other values, disables the
// predicate so any device matches it.
func NewDiskFilter(pathPrefixes, models, types []string) *Filter {
	if len(pathPrefixes) == 0 {
		pathPrefixes = DefaultDiskPathPrefixes
	}
	if len(models) == 0 {
		models = DefaultDiskModels
	}
	if len(types) == 0 {
		types = DefaultDiskTypes
	}
	filter := &Filter{}
	if !containsWildcard(pathPrefixes) {
		filter.Filters = append(filter.Filters, NewPathFilter(pathPrefixes...))
	}
	if !containsWildcard(models) {
		filter.Filters = append(filter.Filters, NewModelFilter(models...))
	}
	if !containsWildcard(types) {
		filter.Filters = append(filter.Filters, NewTypeFilter(types...))
	}
	return filter
}

func containsWildcard(values []string) bool {
	for _, v := range values {
		if strings.TrimSpace(v) == Wildcard {
			return true
		}
	}
	return false
}

// FilterPredicate defines a predicate for filtering devices.
type FilterPredicate interface {
	Match(device block.Device) bool
}

// Filter holds multiple filters and matches if all contained filters match.
type Filter struct {
	Filters []FilterPredicate
}

func (f *Filter) Match(device block.Device) bool {
	for _, filter := range f.Filters {
		if !filter.Match(device) {
			return false
		}
	}
	return true
}

func NewPathFilter(prefixes ...string) *PathFilter {
	return &PathFilter{Paths: prefixes}
}

// PathFilter matches devices by path prefix.
type PathFilter struct {
	Paths []string
}

func (f *PathFilter) Match(device block.Device) bool {
	for _, path := range f.Paths {
		if strings.HasPrefix(device.Path, path) {
			return true
		}
	}
	return false
}

func NewTypeFilter(types ...string) *TypeFilter {
	return &TypeFilter{Types: types}
}

// TypeFilter matches devices by type.
type TypeFilter struct {
	Types []string
}

func (f *TypeFilter) Match(device block.Device) bool {
	for _, t := range f.Types {
		if strings.EqualFold(device.Type, t) {
			return true
		}
	}
	return false
}

func NewModelFilter(models ...string) *ModelFilter {
	modelsMap := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		model = strings.ToLower(model)
		modelsMap[model] = struct{}{}
	}
	return &ModelFilter{models: modelsMap}
}

// ModelFilter matches devices by model.
type ModelFilter struct {
	models map[string]struct{}
}

func (f *ModelFilter) Match(device block.Device) bool {
	deviceModel := strings.TrimSpace(device.Model)
	deviceModel = strings.ToLower(deviceModel)
	_, exists := f.models[deviceModel]
	return exists
}
