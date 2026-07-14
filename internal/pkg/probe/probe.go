// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package probe

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"local-csi-driver/internal/pkg/block"
)

var (
	// ErrNoDevicesFound is returned when no devices are found.
	ErrNoDevicesFound = fmt.Errorf("no devices found")
)

// DiskAdoptionPolicy controls whether formatted non-LVM devices may be
// destructively prepared for LVM use.
type DiskAdoptionPolicy string

const (
	DiskAdoptionPolicyNone          DiskAdoptionPolicy = "none"
	DiskAdoptionPolicyWipeUnmounted DiskAdoptionPolicy = "wipe-unmounted"
	DiskAdoptionPolicyWipeMounted   DiskAdoptionPolicy = "wipe-mounted"
	defaultDiskAdoptionPolicy                          = DiskAdoptionPolicyNone
)

func ParseDiskAdoptionPolicy(value string) (DiskAdoptionPolicy, error) {
	switch DiskAdoptionPolicy(value) {
	case DiskAdoptionPolicyNone:
		return DiskAdoptionPolicyNone, nil
	case DiskAdoptionPolicyWipeUnmounted:
		return DiskAdoptionPolicyWipeUnmounted, nil
	case DiskAdoptionPolicyWipeMounted:
		return DiskAdoptionPolicyWipeMounted, nil
	default:
		return "", fmt.Errorf("invalid disk adoption policy %q", value)
	}
}

//go:generate mockgen -copyright_file ../../../hack/mockgen_copyright.txt -destination=mock_probe.go -mock_names=Interface=Mock -package=probe -source=probe.go Interface
type Interface interface {
	ScanAvailableDevices(ctx context.Context, filter *Filter) (*block.DeviceList, error)
}

var _ Interface = &deviceScanner{}

// deviceScanner is a struct that implements the DeviceScanner interface.
type deviceScanner struct {
	block.Interface
	adoptionPolicy DiskAdoptionPolicy
}

// New creates a new deviceScanner instance.
func New(b block.Interface, opts ...Option) Interface {
	scanner := &deviceScanner{
		Interface:      b,
		adoptionPolicy: defaultDiskAdoptionPolicy,
	}
	for _, opt := range opts {
		opt(scanner)
	}
	return scanner
}

type Option func(*deviceScanner)

func WithDiskAdoptionPolicy(policy DiskAdoptionPolicy) Option {
	return func(scanner *deviceScanner) {
		scanner.adoptionPolicy = policy
	}
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

		adopted, err := m.adoptFormattedDevice(ctx, device)
		if err != nil {
			return nil, err
		}
		if adopted {
			log.V(1).Info("formatted non-LVM device adopted for LVM use", "device", device)
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

func (m *deviceScanner) adoptFormattedDevice(ctx context.Context, device block.Device) (bool, error) {
	log := log.FromContext(ctx)
	switch m.adoptionPolicy {
	case DiskAdoptionPolicyNone:
		return false, nil
	case DiskAdoptionPolicyWipeUnmounted:
		err := m.AdoptDevice(ctx, device, block.AdoptDeviceOptions{AllowMounted: false})
		if err == nil {
			return true, nil
		}
		if errors.Is(err, block.ErrDeviceMounted) {
			log.V(1).Info("formatted non-LVM device is mounted, skipping adoption", "device", device)
			return false, nil
		}
		return false, fmt.Errorf("failed to adopt formatted device %s: %w", device.Path, err)
	case DiskAdoptionPolicyWipeMounted:
		if err := m.AdoptDevice(ctx, device, block.AdoptDeviceOptions{AllowMounted: true}); err != nil {
			return false, fmt.Errorf("failed to adopt formatted device %s: %w", device.Path, err)
		}
		return true, nil
	default:
		return false, fmt.Errorf("invalid disk adoption policy %q", m.adoptionPolicy)
	}
}
