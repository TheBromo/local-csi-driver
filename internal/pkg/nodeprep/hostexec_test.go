// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestHostRunnerUsesHostNamespaces(t *testing.T) {
	t.Setenv("DM_DISABLE_UDEV", "1")

	var gotPath string
	var gotArgs []string
	runner := &HostRunner{
		nsenterPath: "/usr/bin/nsenter",
		commandContext: func(ctx context.Context, path string, args ...string) *exec.Cmd {
			gotPath = path
			gotArgs = append([]string(nil), args...)
			return exec.CommandContext(ctx, "sh", "-c", `printf 'host-output:%s' "${DM_DISABLE_UDEV:-}"`)
		},
	}

	output, err := runner.Run(context.Background(), Command{Name: "lsblk", Args: []string{"--json"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if string(output.Stdout) != "host-output:" {
		t.Fatalf("Run() stdout = %q, want host environment without DM_DISABLE_UDEV", output.Stdout)
	}
	if gotPath != "/usr/bin/nsenter" {
		t.Fatalf("command path = %q, want /usr/bin/nsenter", gotPath)
	}
	wantArgs := []string{
		"--target", "1",
		"--root",
		"--wd",
		"--mount",
		"--uts",
		"--ipc",
		"--net",
		"--pid",
		"--",
		"lsblk",
		"--json",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("nsenter args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestHostRunnerReturnsExitCodeAndStderr(t *testing.T) {
	t.Parallel()

	runner := &HostRunner{
		nsenterPath: "nsenter",
		commandContext: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", `printf 'unsafe state' >&2; exit 7`)
		},
	}

	_, err := runner.Run(context.Background(), Command{Name: "wipefs", Args: []string{fixtureResourceDisk}})
	if err == nil {
		t.Fatal("Run() error = nil, want command failure")
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("Run() error type = %T, want *CommandError", err)
	}
	if commandErr.ExitCode != 7 || commandErr.Stderr != "unsafe state" {
		t.Fatalf("CommandError = %#v, want exit 7 and stderr", commandErr)
	}
	if !IsExitCode(err, 7) {
		t.Fatal("IsExitCode(error, 7) = false, want true")
	}
}

func TestFileLockerSerializesPreparation(t *testing.T) {
	t.Parallel()

	locker := NewFileLocker(t.TempDir() + "/nodeprep.lock")
	locker.retryInterval = time.Millisecond
	first, err := locker.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire(first) error = %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := locker.Acquire(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire(contended) error = %v, want context deadline", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	second, err := locker.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire(after release) error = %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release(second) error = %v", err)
	}
}
