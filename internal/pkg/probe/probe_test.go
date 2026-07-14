// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package probe

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"local-csi-driver/internal/pkg/block"
)

func TestParseDiskAdoptionPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    DiskAdoptionPolicy
		wantErr bool
	}{
		{name: "none", value: "none", want: DiskAdoptionPolicyNone},
		{name: "wipe unmounted", value: "wipe-unmounted", want: DiskAdoptionPolicyWipeUnmounted},
		{name: "wipe mounted", value: "wipe-mounted", want: DiskAdoptionPolicyWipeMounted},
		{name: "invalid", value: "destroy", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseDiskAdoptionPolicy(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseDiskAdoptionPolicy() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseDiskAdoptionPolicy() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScanAvailableDevicesAdoptionPolicy(t *testing.T) {
	t.Parallel()

	errTest := errors.New("wipe failed")
	formattedDevice := block.Device{
		Name:  "sdb",
		Path:  "/dev/sdb",
		Type:  "disk",
		Model: "Virtual Disk",
		Size:  1024,
	}
	mountedFormattedDevice := formattedDevice
	mountedFormattedDevice.Children = []block.Device{
		{Path: "/dev/sdb1", Type: "part", Mountpoints: []string{"/mnt"}},
	}
	partitionedUnmountedDevice := formattedDevice
	partitionedUnmountedDevice.Children = []block.Device{
		{Path: "/dev/sdb1", Type: "part"},
	}

	tests := []struct {
		name      string
		policy    DiskAdoptionPolicy
		device    block.Device
		expect    func(*block.Mock)
		wantErr   bool
		wantCount int
		wantAdopt bool
	}{
		{
			name:   "default skips formatted non-lvm device",
			policy: DiskAdoptionPolicyNone,
			device: formattedDevice,
			expect: func(m *block.Mock) {
				m.EXPECT().IsFormatted("/dev/sdb").Return(true, nil)
				m.EXPECT().IsLVM2("/dev/sdb").Return(false, nil)
			},
		},
		{
			name:   "default skips unformatted parent with mounted child",
			policy: DiskAdoptionPolicyNone,
			device: mountedFormattedDevice,
			expect: func(m *block.Mock) {
				m.EXPECT().IsFormatted("/dev/sdb").Return(false, nil)
			},
		},
		{
			name:   "unformatted parent without children remains available",
			policy: DiskAdoptionPolicyNone,
			device: formattedDevice,
			expect: func(m *block.Mock) {
				m.EXPECT().IsFormatted("/dev/sdb").Return(false, nil)
			},
			wantCount: 1,
		},
		{
			name:   "wipe unmounted adopts unformatted parent with unmounted child",
			policy: DiskAdoptionPolicyWipeUnmounted,
			device: partitionedUnmountedDevice,
			expect: func(m *block.Mock) {
				m.EXPECT().IsFormatted("/dev/sdb").Return(false, nil)
				m.EXPECT().AdoptDevice(gomock.Any(), partitionedUnmountedDevice, block.AdoptDeviceOptions{AllowMounted: false}).Return(nil)
			},
			wantCount: 1,
			wantAdopt: true,
		},
		{
			name:   "wipe unmounted skips unformatted parent with mounted child",
			policy: DiskAdoptionPolicyWipeUnmounted,
			device: mountedFormattedDevice,
			expect: func(m *block.Mock) {
				m.EXPECT().IsFormatted("/dev/sdb").Return(false, nil)
				m.EXPECT().AdoptDevice(gomock.Any(), mountedFormattedDevice, block.AdoptDeviceOptions{AllowMounted: false}).Return(block.ErrDeviceMounted)
			},
		},
		{
			name:   "wipe mounted adopts unformatted parent with mounted child",
			policy: DiskAdoptionPolicyWipeMounted,
			device: mountedFormattedDevice,
			expect: func(m *block.Mock) {
				m.EXPECT().IsFormatted("/dev/sdb").Return(false, nil)
				m.EXPECT().AdoptDevice(gomock.Any(), mountedFormattedDevice, block.AdoptDeviceOptions{AllowMounted: true}).Return(nil)
			},
			wantCount: 1,
			wantAdopt: true,
		},
		{
			name:   "wipe unmounted adopts unmounted formatted non-lvm device",
			policy: DiskAdoptionPolicyWipeUnmounted,
			device: formattedDevice,
			expect: func(m *block.Mock) {
				m.EXPECT().IsFormatted("/dev/sdb").Return(true, nil)
				m.EXPECT().IsLVM2("/dev/sdb").Return(false, nil)
				m.EXPECT().AdoptDevice(gomock.Any(), formattedDevice, block.AdoptDeviceOptions{AllowMounted: false}).Return(nil)
			},
			wantCount: 1,
			wantAdopt: true,
		},
		{
			name:   "wipe unmounted skips mounted formatted non-lvm device",
			policy: DiskAdoptionPolicyWipeUnmounted,
			device: mountedFormattedDevice,
			expect: func(m *block.Mock) {
				m.EXPECT().IsFormatted("/dev/sdb").Return(true, nil)
				m.EXPECT().IsLVM2("/dev/sdb").Return(false, nil)
				m.EXPECT().AdoptDevice(gomock.Any(), mountedFormattedDevice, block.AdoptDeviceOptions{AllowMounted: false}).Return(block.ErrDeviceMounted)
			},
		},
		{
			name:   "wipe mounted adopts mounted formatted non-lvm device",
			policy: DiskAdoptionPolicyWipeMounted,
			device: mountedFormattedDevice,
			expect: func(m *block.Mock) {
				m.EXPECT().IsFormatted("/dev/sdb").Return(true, nil)
				m.EXPECT().IsLVM2("/dev/sdb").Return(false, nil)
				m.EXPECT().AdoptDevice(gomock.Any(), mountedFormattedDevice, block.AdoptDeviceOptions{AllowMounted: true}).Return(nil)
			},
			wantCount: 1,
			wantAdopt: true,
		},
		{
			name:   "adoption error fails scan",
			policy: DiskAdoptionPolicyWipeMounted,
			device: formattedDevice,
			expect: func(m *block.Mock) {
				m.EXPECT().IsFormatted("/dev/sdb").Return(true, nil)
				m.EXPECT().IsLVM2("/dev/sdb").Return(false, nil)
				m.EXPECT().AdoptDevice(gomock.Any(), formattedDevice, block.AdoptDeviceOptions{AllowMounted: true}).Return(errTest)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctrl := gomock.NewController(t)
			mockBlock := block.NewMock(ctrl)
			mockBlock.EXPECT().GetDevices(gomock.Any()).Return(&block.DeviceList{Devices: []block.Device{tt.device}}, nil)
			tt.expect(mockBlock)

			scanner := New(mockBlock, WithDiskAdoptionPolicy(tt.policy))
			got, err := scanner.ScanAvailableDevices(context.Background(), NewDiskFilter([]string{"/dev/sd"}, []string{"Virtual Disk"}, []string{"disk"}))
			if tt.wantErr {
				if err == nil {
					t.Fatal("ScanAvailableDevices() error = nil, want error")
				}
				return
			}
			if tt.wantCount == 0 {
				if !errors.Is(err, ErrNoDevicesFound) {
					t.Fatalf("ScanAvailableDevices() error = %v, want ErrNoDevicesFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ScanAvailableDevices() error = %v", err)
			}
			if len(got.Devices) != tt.wantCount {
				t.Fatalf("ScanAvailableDevices() returned %d devices, want %d", len(got.Devices), tt.wantCount)
			}
			if got.Devices[0].Adopted != tt.wantAdopt {
				t.Fatalf("ScanAvailableDevices() returned adopted=%v, want %v", got.Devices[0].Adopted, tt.wantAdopt)
			}
		})
	}
}
