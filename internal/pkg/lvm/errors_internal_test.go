// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lvm

import (
	"errors"
	"fmt"
	"testing"
)

func TestGetErrorType(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{
			name:    "volume group not found",
			input:   "Volume group \"vg0\" not found",
			wantErr: ErrNotFound,
		},
		{
			name:    "failed to find logical volume",
			input:   "Failed to find logical volume vg0/lv0",
			wantErr: ErrNotFound,
		},
		{
			// vgcreate blocked by a leftover /dev node with no LVM metadata.
			name:    "stale device node blocks vgcreate",
			input:   "/dev/vg0: already exists in filesystem",
			wantErr: ErrStaleDeviceNode,
		},
		{
			// A genuine duplicate volume group in LVM metadata.
			name:    "volume group already exists in metadata",
			input:   "A volume group called vg0 already exists.",
			wantErr: ErrAlreadyExists,
		},
		{
			name:    "filesystem in use",
			input:   "Can't open /dev/loop0 exclusively. Device contains a filesystem in use.",
			wantErr: ErrInUse,
		},
		{
			name:    "insufficient free space",
			input:   "Volume group \"vg0\" has insufficient free space",
			wantErr: ErrResourceExhausted,
		},
		{
			name:    "physical volume already in volume group",
			input:   "Physical volume /dev/loop0 is already in volume group vg0",
			wantErr: ErrPVAlreadyInVolumeGroup,
		},
		{
			name:    "unrecognized error is passed through",
			input:   "some unexpected lvm failure",
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getErrorType(errors.New(tt.input))
			if tt.wantErr == nil {
				if got.Error() != tt.input {
					t.Fatalf("expected passthrough %q, got %q", tt.input, got.Error())
				}
				return
			}
			if !errors.Is(got, tt.wantErr) {
				t.Fatalf("expected error to wrap %v, got %v", tt.wantErr, got)
			}
			// The stale-node case must not be conflated with ErrAlreadyExists.
			if tt.wantErr == ErrStaleDeviceNode && errors.Is(got, ErrAlreadyExists) {
				t.Fatalf("stale device node error must not be categorized as ErrAlreadyExists: %v", got)
			}
		})
	}
}

func TestGetErrorTypeLVMCommandError(t *testing.T) {
	t.Parallel()

	errExitStatus5 := errors.New("exit status 5")
	tests := []struct {
		name    string
		err     error
		wantErr error
	}{
		{
			name: "vgs exit status 5 is not found",
			err: &lvmCommandError{
				err:      errExitStatus5,
				cmdArgs:  []string{"vgs", "--reportformat=json", "containerstorage"},
				exitCode: 5,
			},
			wantErr: ErrNotFound,
		},
		{
			name: "lvs exit status 5 is not found",
			err: &lvmCommandError{
				err:      errExitStatus5,
				cmdArgs:  []string{"lvs", "--reportformat=json", "containerstorage/missing-lv"},
				exitCode: 5,
			},
			wantErr: ErrNotFound,
		},
		{
			name: "pvs exit status 5 is not found",
			err: &lvmCommandError{
				err:      errExitStatus5,
				cmdArgs:  []string{"pvs", "--reportformat=json", "/dev/missing"},
				exitCode: 5,
			},
			wantErr: ErrNotFound,
		},
		{
			name: "vgcreate exit status 5 passes through",
			err: &lvmCommandError{
				err:      errExitStatus5,
				cmdArgs:  []string{"vgcreate", "containerstorage", "/dev/sdb"},
				exitCode: 5,
			},
		},
		{
			name: "vgs different exit code passes through",
			err: &lvmCommandError{
				err:      errors.New("exit status 3"),
				cmdArgs:  []string{"vgs", "--reportformat=json", "containerstorage"},
				exitCode: 3,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := getErrorType(tt.err)
			if tt.wantErr == nil {
				if got != tt.err {
					t.Fatalf("expected passthrough error %v, got %v", tt.err, got)
				}
				return
			}
			if !errors.Is(got, tt.wantErr) {
				t.Fatalf("expected error to wrap %v, got %v", tt.wantErr, got)
			}
		})
	}
}

func TestLVMCommandError(t *testing.T) {
	t.Parallel()

	errCommand := errors.New("exit status 5")
	cmdArgs := []string{"vgs", "vg0"}
	err := newLVMCommandError(errCommand, "  Volume group \"vg0\" not found\n", cmdArgs)
	if !errors.Is(err, errCommand) {
		t.Fatalf("expected command error to wrap %v, got %v", errCommand, err)
	}
	if err.Error() != "exit status 5: Volume group \"vg0\" not found" {
		t.Fatalf("unexpected error string: %q", err.Error())
	}
	if err.exitCode != -1 {
		t.Fatalf("exitCode = %d, want -1 for non-exec error", err.exitCode)
	}
	cmdArgs[0] = "mutated"
	if err.cmdArgs[0] != "vgs" {
		t.Fatalf("cmdArgs were not copied: %v", err.cmdArgs)
	}
}

// Ensure the sentinel errors remain distinct.
func TestStaleDeviceNodeDistinctFromAlreadyExists(t *testing.T) {
	wrapped := fmt.Errorf("%w: detail", ErrStaleDeviceNode)
	if errors.Is(wrapped, ErrAlreadyExists) {
		t.Fatal("ErrStaleDeviceNode must not match ErrAlreadyExists")
	}
	if !errors.Is(wrapped, ErrStaleDeviceNode) {
		t.Fatal("wrapped error should match ErrStaleDeviceNode")
	}
}
