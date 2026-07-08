// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lvm

import (
	"reflect"
	"testing"

	"local-csi-driver/internal/pkg/block"
)

func TestSplitParam(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected []string
	}{
		{
			name:     "empty value",
			value:    "",
			expected: nil,
		},
		{
			name:     "whitespace only",
			value:    "   ",
			expected: nil,
		},
		{
			name:     "commas only",
			value:    ",,",
			expected: nil,
		},
		{
			name:     "single value",
			value:    "/dev/nvme",
			expected: []string{"/dev/nvme"},
		},
		{
			name:     "multiple values with whitespace",
			value:    " /dev/nvme , /dev/sd ",
			expected: []string{"/dev/nvme", "/dev/sd"},
		},
		{
			name:     "empty entries dropped",
			value:    "disk,,loop,",
			expected: []string{"disk", "loop"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := splitParam(tt.value)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("splitParam(%q) = %v, want %v", tt.value, result, tt.expected)
			}
		})
	}
}

func TestDiskFilterFromParams(t *testing.T) {
	defaultDevice := block.Device{Path: "/dev/nvme0n1", Type: "disk", Model: "Microsoft NVMe Direct Disk"}
	customDevice := block.Device{Path: "/dev/sda", Type: "disk", Model: "Samsung SSD"}

	tests := []struct {
		name     string
		params   map[string]string
		device   block.Device
		expected bool
	}{
		{
			name:     "nil params use defaults",
			params:   nil,
			device:   defaultDevice,
			expected: true,
		},
		{
			name:     "nil params reject non-default device",
			params:   nil,
			device:   customDevice,
			expected: false,
		},
		{
			name:     "empty params use defaults",
			params:   map[string]string{},
			device:   defaultDevice,
			expected: true,
		},
		{
			name: "empty values fall back to defaults",
			params: map[string]string{
				DiskPathPrefixesParam: "",
				DiskModelsParam:       "  ",
				DiskTypesParam:        ",,",
			},
			device:   defaultDevice,
			expected: true,
		},
		{
			name: "custom params match custom device",
			params: map[string]string{
				DiskPathPrefixesParam: "/dev/sd",
				DiskModelsParam:       "Samsung SSD",
			},
			device:   customDevice,
			expected: true,
		},
		{
			name: "custom params reject default device",
			params: map[string]string{
				DiskPathPrefixesParam: "/dev/sd",
				DiskModelsParam:       "Samsung SSD",
			},
			device:   defaultDevice,
			expected: false,
		},
		{
			name: "comma-separated values with whitespace",
			params: map[string]string{
				DiskPathPrefixesParam: "/dev/nvme, /dev/sd",
				DiskModelsParam:       "Microsoft NVMe Direct Disk, Samsung SSD",
			},
			device:   customDevice,
			expected: true,
		},
		{
			name: "partial override keeps default models",
			params: map[string]string{
				DiskPathPrefixesParam: "/dev/sd",
			},
			device:   customDevice,
			expected: false,
		},
		{
			name: "unrelated params ignored",
			params: map[string]string{
				VolumeGroupNameParam: "custom-vg",
			},
			device:   defaultDevice,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := diskFilterFromParams(tt.params)
			result := filter.Match(tt.device)
			if result != tt.expected {
				t.Errorf("Match(%v) = %v, want %v", tt.device, result, tt.expected)
			}
		})
	}
}
