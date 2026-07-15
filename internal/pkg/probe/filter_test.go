// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package probe

import (
	"testing"

	"local-csi-driver/internal/pkg/block"
)

func TestPathFilter(t *testing.T) {
	tests := []struct {
		name     string
		paths    []string
		device   block.Device
		expected bool
	}{
		{
			name:     "match path prefix",
			paths:    []string{"/dev/sd"},
			device:   block.Device{Path: "/dev/sda"},
			expected: true,
		},
		{
			name:     "no match path prefix",
			paths:    []string{"/dev/sd"},
			device:   block.Device{Path: "/dev/nvme0n1"},
			expected: false,
		},
		{
			name:     "match any of multiple prefixes",
			paths:    []string{"/dev/sd", "/dev/nvme"},
			device:   block.Device{Path: "/dev/nvme0n1"},
			expected: true,
		},
		{
			name:     "no match with multiple prefixes",
			paths:    []string{"/dev/sd", "/dev/nvme"},
			device:   block.Device{Path: "/dev/loop0"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewPathFilter(tt.paths...)
			result := filter.Match(tt.device)
			if result != tt.expected {
				t.Errorf("Match(%v) = %v, want %v", tt.device, result, tt.expected)
			}
		})
	}
}

func TestTypeFilter(t *testing.T) {
	tests := []struct {
		name     string
		types    []string
		device   block.Device
		expected bool
	}{
		{
			name:     "match type",
			types:    []string{"SSD"},
			device:   block.Device{Type: "ssd"},
			expected: true,
		},
		{
			name:     "no match type",
			types:    []string{"HDD"},
			device:   block.Device{Type: "ssd"},
			expected: false,
		},
		{
			name:     "match any of multiple types",
			types:    []string{"disk", "loop"},
			device:   block.Device{Type: "loop"},
			expected: true,
		},
		{
			name:     "no match with multiple types",
			types:    []string{"disk", "loop"},
			device:   block.Device{Type: "part"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewTypeFilter(tt.types...)
			result := filter.Match(tt.device)
			if result != tt.expected {
				t.Errorf("Match(%v) = %v, want %v", tt.device, result, tt.expected)
			}
		})
	}
}

func TestModelFilter(t *testing.T) {
	tests := []struct {
		name     string
		models   []string
		device   block.Device
		expected bool
	}{
		{
			name:     "match model",
			models:   []string{"Samsung v2", "Samsung"},
			device:   block.Device{Model: " Samsung "},
			expected: true,
		},
		{
			name:     "match model",
			models:   []string{"Samsung v2", "Samsung"},
			device:   block.Device{Model: " Samsung v2 "},
			expected: true,
		},
		{
			name:     "no match model",
			models:   []string{"Intel"},
			device:   block.Device{Model: "Samsung"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewModelFilter(tt.models...)
			result := filter.Match(tt.device)
			if result != tt.expected {
				t.Errorf("Match(%v) = %v, want %v", tt.device, result, tt.expected)
			}
		})
	}
}

func TestFilter(t *testing.T) {
	tests := []struct {
		name     string
		filters  []FilterPredicate
		device   block.Device
		expected bool
	}{
		{
			name: "all filters match",
			filters: []FilterPredicate{
				NewPathFilter("/dev/sd"),
				NewTypeFilter("SSD"),
				NewModelFilter("Samsung", "Samsung v2"),
			},
			device:   block.Device{Path: "/dev/sda", Type: "ssd", Model: "Samsung"},
			expected: true,
		},
		{
			name: "one filter does not match",
			filters: []FilterPredicate{
				NewPathFilter("/dev/sd"),
				NewTypeFilter("SSD"),
				NewModelFilter("Intel"),
			},
			device:   block.Device{Path: "/dev/sda", Type: "ssd", Model: "Samsung"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &Filter{Filters: tt.filters}
			result := filter.Match(tt.device)
			if result != tt.expected {
				t.Errorf("Match(%v) = %v, want %v", tt.device, result, tt.expected)
			}
		})
	}
}

func TestNewDiskFilter(t *testing.T) {
	tests := []struct {
		name         string
		pathPrefixes []string
		models       []string
		types        []string
		device       block.Device
		expected     bool
	}{
		{
			name:     "nil slices use defaults",
			device:   block.Device{Path: "/dev/nvme0n1", Type: "disk", Model: "Microsoft NVMe Direct Disk"},
			expected: true,
		},
		{
			name:     "nil slices use defaults, non-default device rejected",
			device:   block.Device{Path: "/dev/sda", Type: "disk", Model: "Samsung"},
			expected: false,
		},
		{
			name:         "custom values match",
			pathPrefixes: []string{"/dev/sd"},
			models:       []string{"Samsung"},
			types:        []string{"disk"},
			device:       block.Device{Path: "/dev/sda", Type: "disk", Model: "Samsung"},
			expected:     true,
		},
		{
			name:         "custom values reject default device",
			pathPrefixes: []string{"/dev/sd"},
			models:       []string{"Samsung"},
			types:        []string{"disk"},
			device:       block.Device{Path: "/dev/nvme0n1", Type: "disk", Model: "Microsoft NVMe Direct Disk"},
			expected:     false,
		},
		{
			name:     "partial override keeps defaults for other predicates",
			models:   []string{"Samsung"},
			device:   block.Device{Path: "/dev/nvme0n1", Type: "disk", Model: "Samsung"},
			expected: true,
		},
		{
			name:     "partial override still applies default path prefix",
			models:   []string{"Samsung"},
			device:   block.Device{Path: "/dev/sda", Type: "disk", Model: "Samsung"},
			expected: false,
		},
		{
			name:         "wildcard path matches any path but enforces default models",
			pathPrefixes: []string{"*"},
			device:       block.Device{Path: "/dev/xvda", Type: "disk", Model: "Microsoft NVMe Direct Disk"},
			expected:     true,
		},
		{
			name:         "wildcard path still rejects non-default model",
			pathPrefixes: []string{"*"},
			device:       block.Device{Path: "/dev/xvda", Type: "disk", Model: "Samsung"},
			expected:     false,
		},
		{
			name:         "wildcard models accepts any model",
			pathPrefixes: []string{"/dev/sd"},
			models:       []string{"*"},
			device:       block.Device{Path: "/dev/sda", Type: "disk", Model: "Samsung"},
			expected:     true,
		},
		{
			name:     "wildcard types accepts any type",
			models:   []string{"Samsung"},
			types:    []string{"*"},
			device:   block.Device{Path: "/dev/nvme0n1", Type: "loop", Model: "Samsung"},
			expected: true,
		},
		{
			name:         "wildcard everywhere matches arbitrary device",
			pathPrefixes: []string{"*"},
			models:       []string{"*"},
			types:        []string{"*"},
			device:       block.Device{Path: "/dev/weird0", Type: "rom", Model: "Anything"},
			expected:     true,
		},
		{
			name:         "wildcard mixed with other values disables the predicate",
			pathPrefixes: []string{"/dev/sd"},
			models:       []string{"Intel", "*"},
			device:       block.Device{Path: "/dev/sda", Type: "disk", Model: "Samsung"},
			expected:     true,
		},
		{
			name:         "padded wildcard is recognized",
			pathPrefixes: []string{"/dev/sd"},
			models:       []string{" * "},
			device:       block.Device{Path: "/dev/sda", Type: "disk", Model: "Samsung"},
			expected:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewDiskFilter(tt.pathPrefixes, tt.models, tt.types)
			result := filter.Match(tt.device)
			if result != tt.expected {
				t.Errorf("Match(%v) = %v, want %v", tt.device, result, tt.expected)
			}
		})
	}
}

func TestEphemeralDiskFilter(t *testing.T) {
	tests := []struct {
		name     string
		device   block.Device
		expected bool
	}{
		{
			// Standard_NC40ads_H100_v5 nodes have a different model name
			// for the ephemeral disk. This test case is to ensure that
			// the filter matches the model name for these nodes.
			name: "match disk for v2 direct disk nodes",
			device: block.Device{
				Path:  "/dev/nvme0n1",
				Type:  "disk",
				Model: "Microsoft NVMe Direct Disk v2           ",
			},
			expected: true,
		},
		{
			name: "match disk for direct disk nodes",
			device: block.Device{
				Path:  "/dev/nvme0n1",
				Type:  "disk",
				Model: "Microsoft NVMe Direct Disk           ",
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EphemeralDiskFilter.Match(tt.device)
			if result != tt.expected {
				t.Errorf("Match(%v) = %v, want %v", tt.device, result, tt.expected)
			}
		})
	}
}
