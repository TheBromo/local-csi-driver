// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package probe

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"local-csi-driver/internal/pkg/block"
)

var (
	// ErrNoDevicesFound is returned when no devices are found.
	ErrNoDevicesFound = fmt.Errorf("no devices found")
)

//go:generate mockgen -copyright_file ../../../hack/mockgen_copyright.txt -destination=mock_probe.go -mock_names=Interface=Mock -package=probe -source=probe.go Interface
type Interface interface {
	ScanAvailableDevices(ctx context.Context, filter *Filter) (*block.DeviceList, error)
}

var _ Interface = &deviceScanner{}

// deviceScanner is a struct that implements the DeviceScanner interface.
type deviceScanner struct {
	block.Interface
}

// New creates a new deviceScanner instance.
func New(b block.Interface) Interface {
	return &deviceScanner{b}
}

// ScanAvailableDevices retrieves devices matching the filter that are
// unformatted or already LVM physical volumes. A nil filter falls back to
// the default EphemeralDiskFilter.
func (m *deviceScanner) ScanAvailableDevices(ctx context.Context, filter *Filter) (*block.DeviceList, error) {
	log := log.FromContext(ctx)
	if filter == nil {
		filter = EphemeralDiskFilter
	}
	devices, err := m.GetDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get devices: %w", err)
	}

	var availableDevices []block.Device
	for _, device := range devices.Devices {
		if !filter.Match(device) {
			log.V(3).Info("device filtered out", "device", device)
			continue
		}
		isFormatted, err := m.IsFormatted(device.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to check if device is unformatted: %w", err)
		}
		if !isFormatted {
			log.V(3).Info("unformatted device found", "device", device)
			availableDevices = append(availableDevices, device)
			continue
		}

		isLVM2, err := m.IsLVM2(device.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to check if device is LVM2: %w", err)
		}
		if isLVM2 {
			log.V(3).Info("device is LVM physical volume, adding to available", "device", device)
			availableDevices = append(availableDevices, device)
			continue
		}

		log.V(3).Info("device is formatted and not lvm2, skipping", "device", device)
	}

	if len(availableDevices) == 0 {
		return nil, ErrNoDevicesFound
	}
	return &block.DeviceList{Devices: availableDevices}, nil
}
