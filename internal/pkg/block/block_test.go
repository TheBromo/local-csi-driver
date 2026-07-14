// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package block

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	utilexec "k8s.io/utils/exec"
	fakeexec "k8s.io/utils/exec/testing"
)

func TestParseLsblkOutput(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    *DeviceList
		wantErr bool
	}{
		{
			name: "Valid lsblk output",
			output: `{
                "blockdevices": [
                    {
                        "name": "sda",
                        "path": "/dev/sda",
                        "maj:min": "8:0",
                        "rm": false,
                        "type": "disk",
                        "mountpoint": "/mnt/data",
                        "mountpoints": ["/mnt/data"],
                        "model": "Samsung SSD",
                        "serial": "123456789",
                        "size": 536870912000,
                        "children": [
                            {
                                "name": "sda1",
                                "path": "/dev/sda1",
                                "type": "part",
                                "size": 536869863424,
                                "mountpoints": ["/mnt/data/child"]
                            }
                        ]
                    },
                    {
                        "name": "sdb",
                        "path": "/dev/sdb",
                        "maj:min": "8:16",
                        "rm": false,
                        "type": "disk",
                        "mountpoint": "/mnt/backup",
                        "mountpoints": ["/mnt/backup"],
                        "model": "WD HDD",
                        "serial": "987654321",
                        "size": 1099511627776
                    },
                    {
                        "name": "sdc",
                        "path": "/dev/sdc",
                        "maj:min": "8:32",
                        "rm": true,
                        "type": "disk",
                        "mountpoint": "/mnt/usb",
                        "mountpoints": ["/mnt/usb"],
                        "model": "SanDisk USB",
                        "serial": "1122334455",
                        "size": 68719476736
                    }
                ]
            }`,
			want: &DeviceList{
				Devices: []Device{
					{
						Name:        "sda",
						Path:        "/dev/sda",
						MajMin:      "8:0",
						Removable:   false,
						Type:        "disk",
						Mountpoint:  "/mnt/data",
						Mountpoints: []string{"/mnt/data"},
						Model:       "Samsung SSD",
						Serial:      "123456789",
						Size:        536870912000,
						Children: []Device{
							{
								Name:        "sda1",
								Path:        "/dev/sda1",
								Type:        "part",
								Mountpoints: []string{"/mnt/data/child"},
								Size:        536869863424,
							},
						},
					},
					{
						Name:        "sdb",
						Path:        "/dev/sdb",
						MajMin:      "8:16",
						Removable:   false,
						Type:        "disk",
						Mountpoint:  "/mnt/backup",
						Mountpoints: []string{"/mnt/backup"},
						Model:       "WD HDD",
						Serial:      "987654321",
						Size:        1099511627776,
					},
					{
						Name:        "sdc",
						Path:        "/dev/sdc",
						MajMin:      "8:32",
						Removable:   true,
						Type:        "disk",
						Mountpoint:  "/mnt/usb",
						Mountpoints: []string{"/mnt/usb"},
						Model:       "SanDisk USB",
						Serial:      "1122334455",
						Size:        68719476736,
					},
				},
			},
			wantErr: false,
		},
		{
			name:    "Empty output",
			output:  `{}`,
			want:    &DeviceList{},
			wantErr: false,
		},
		{
			name:    "Invalid JSON",
			output:  `{invalid json}`,
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLsblkOutput([]byte(tt.output))
			if (err != nil) != tt.wantErr {
				t.Errorf("parseLsblkOutput() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseLsblkOutput() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeviceMountedPaths(t *testing.T) {
	t.Parallel()

	device := Device{
		Path:       "/dev/sdb",
		Mountpoint: "/mnt-parent",
		Mountpoints: []string{
			"",
			"/mnt-parent-duplicate-source",
		},
		Children: []Device{
			{
				Path:        "/dev/sdb1",
				Mountpoints: []string{"/mnt"},
			},
		},
	}

	got := device.MountedPaths()
	want := []string{"/mnt", "/mnt-parent", "/mnt-parent-duplicate-source"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MountedPaths() = %v, want %v", got, want)
	}
}

func TestDevicePaths(t *testing.T) {
	t.Parallel()

	device := Device{
		Path: "/dev/sdb",
		Children: []Device{
			{Path: "/dev/sdb1"},
			{Path: "/dev/sdb2"},
		},
	}

	got := device.DevicePaths()
	want := []string{"/dev/sdb1", "/dev/sdb2", "/dev/sdb"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DevicePaths() = %v, want %v", got, want)
	}
}

func TestAdoptDevice(t *testing.T) {
	t.Parallel()

	device := Device{
		Path: "/dev/sdb",
		Children: []Device{
			{Path: "/dev/sdb1", Mountpoints: []string{"/mnt"}},
		},
	}

	tests := []struct {
		name        string
		opts        AdoptDeviceOptions
		wantErr     error
		wantCommand []scriptedCommand
	}{
		{
			name:    "mounted device rejected unless allowed",
			opts:    AdoptDeviceOptions{AllowMounted: false},
			wantErr: ErrDeviceMounted,
			wantCommand: []scriptedCommand{
				{
					cmd:    "lsblk",
					args:   []string{"--bytes", "--json", "--output-all", "/dev/sdb"},
					output: deviceTreeJSON(),
				},
			},
		},
		{
			name: "mounted device unmounted and wiped when allowed",
			opts: AdoptDeviceOptions{AllowMounted: true},
			wantCommand: []scriptedCommand{
				{
					cmd:    "lsblk",
					args:   []string{"--bytes", "--json", "--output-all", "/dev/sdb"},
					output: deviceTreeJSON(),
				},
				{cmd: "nsenter", args: []string{"--target", "1", "--mount", "--", "umount", "/mnt"}},
				{cmd: "wipefs", args: []string{"--all", "--force", "/dev/sdb1"}},
				{cmd: "wipefs", args: []string{"--all", "--force", "/dev/sdb"}},
				{cmd: "blockdev", args: []string{"--rereadpt", "/dev/sdb"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fakeExec := newFakeExec(t, tt.wantCommand)
			b := &block{exec: fakeExec}
			err := b.AdoptDevice(context.Background(), device, tt.opts)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("AdoptDevice() error = %v, want %v", err, tt.wantErr)
			}
			if fakeExec.CommandCalls != len(tt.wantCommand) {
				t.Fatalf("executed %d commands, want %d", fakeExec.CommandCalls, len(tt.wantCommand))
			}
		})
	}
}

type scriptedCommand struct {
	cmd    string
	args   []string
	output []byte
}

func newFakeExec(t *testing.T, commands []scriptedCommand) *fakeexec.FakeExec {
	t.Helper()

	fakeExec := &fakeexec.FakeExec{
		ExactOrder: true,
		LookPathFunc: func(file string) (string, error) {
			return file, nil
		},
	}
	for _, command := range commands {
		fakeCmd := &fakeexec.FakeCmd{
			CombinedOutputScript: []fakeexec.FakeAction{
				func() ([]byte, []byte, error) { return command.output, nil, nil },
			},
		}
		fakeExec.CommandScript = append(fakeExec.CommandScript, func(cmd string, args ...string) utilexec.Cmd {
			if cmd != command.cmd || !slices.Equal(args, command.args) {
				t.Fatalf("command = %s %v, want %s %v", cmd, args, command.cmd, command.args)
			}
			return fakeexec.InitFakeCmd(fakeCmd, command.cmd, command.args...)
		})
	}
	return fakeExec
}

func deviceTreeJSON() []byte {
	return []byte(`{
		"blockdevices": [
			{
				"name": "sdb",
				"path": "/dev/sdb",
				"type": "disk",
				"size": 1546188226560,
				"mountpoints": [null],
				"children": [
					{
						"name": "sdb1",
						"path": "/dev/sdb1",
						"type": "part",
						"size": 1546186129408,
						"mountpoints": ["/mnt"]
					}
				]
			}
		]
	}`)
}
