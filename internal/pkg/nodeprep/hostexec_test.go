// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"context"
	"fmt"
	"strings"
	"testing"

	utilexec "k8s.io/utils/exec"
	fakeexec "k8s.io/utils/exec/testing"
)

// newFakeExec returns a FakeExec whose single command writes stdout/stderr
// and returns err, capturing the executed command line.
func newFakeExec(t *testing.T, stdout, stderr string, err error, gotCmd *string) *fakeexec.FakeExec {
	t.Helper()
	return &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) utilexec.Cmd {
				*gotCmd = cmd + " " + strings.Join(args, " ")
				fakeCmd := &fakeexec.FakeCmd{
					RunScript: []fakeexec.FakeAction{
						func() ([]byte, []byte, error) {
							return []byte(stdout), []byte(stderr), err
						},
					},
				}
				return fakeexec.InitFakeCmd(fakeCmd, cmd, args...)
			},
		},
	}
}

func TestHostRunnerEntersHostNamespaces(t *testing.T) {
	t.Parallel()
	var gotCmd string
	runner := NewHostRunner(newFakeExec(t, "hello\n", "", nil, &gotCmd))

	out, err := runner.Run(context.Background(), "lsblk", "--json", "/dev/sdb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("expected stdout, got %q", out)
	}
	want := "nsenter --target 1 --mount --uts --ipc --net --pid -- lsblk --json /dev/sdb"
	if gotCmd != want {
		t.Errorf("expected command %q, got %q", want, gotCmd)
	}
}

func TestHostRunnerIncludesStderrInError(t *testing.T) {
	t.Parallel()
	var gotCmd string
	runner := NewHostRunner(newFakeExec(t, "", "wipefs: cannot open device\n", fmt.Errorf("exit status 1"), &gotCmd))

	_, err := runner.Run(context.Background(), "wipefs", "--all", "/dev/sdb")
	if err == nil || !strings.Contains(err.Error(), "cannot open device") {
		t.Fatalf("expected stderr in error, got: %v", err)
	}
}

func TestHostRunnerLookPath(t *testing.T) {
	t.Parallel()
	var gotCmd string
	runner := NewHostRunner(newFakeExec(t, "/usr/sbin/lvm\n", "", nil, &gotCmd))

	if err := runner.LookPath(context.Background(), "lvm"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(gotCmd, "sh -c command -v lvm") {
		t.Errorf("expected command -v probe, got %q", gotCmd)
	}

	var failedCmd string
	failing := NewHostRunner(newFakeExec(t, "", "", fmt.Errorf("exit status 127"), &failedCmd))
	if err := failing.LookPath(context.Background(), "mdadm"); err == nil || !strings.Contains(err.Error(), "mdadm") {
		t.Fatalf("expected missing command error, got: %v", err)
	}
}
