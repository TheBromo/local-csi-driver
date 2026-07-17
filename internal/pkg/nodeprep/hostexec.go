// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	utilexec "k8s.io/utils/exec"
)

// Runner executes commands on the host.
//
// The production implementation enters the host namespaces with
// `nsenter --target 1` so commands like lsblk, wipefs and the LVM tools
// operate on the real host state (host udev, host lvm.conf and host
// system.devices), not the container's. Destructive sequencing is tested
// against fakes of this interface.
type Runner interface {
	// Run executes the command on the host and returns its stdout. Stderr is
	// included in the returned error on failure.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// LookPath verifies that the command is available on the host.
	LookPath(ctx context.Context, name string) error
}

// nsenterArgs enter all host namespaces of PID 1. Requires hostPID and a
// privileged security context.
var nsenterArgs = []string{"--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--"}

// hostRunner is the nsenter-based Runner implementation.
type hostRunner struct {
	exec utilexec.Interface
}

var _ Runner = &hostRunner{}

// NewHostRunner returns a Runner that executes commands in the host
// namespaces via nsenter.
func NewHostRunner(e utilexec.Interface) Runner {
	return &hostRunner{exec: e}
}

// Run executes the command in the host namespaces and returns its stdout.
func (h *hostRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	full := make([]string, 0, len(nsenterArgs)+len(args)+1)
	full = append(full, nsenterArgs...)
	full = append(full, name)
	full = append(full, args...)

	var stdout, stderr bytes.Buffer
	cmd := h.exec.CommandContext(ctx, "nsenter", full...)
	cmd.SetStdout(&stdout)
	cmd.SetStderr(&stderr)
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("host command %q failed: %w: %s", name+" "+strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// LookPath verifies the command is available on the host by resolving it
// with the shell builtin `command -v` inside the host mount namespace.
func (h *hostRunner) LookPath(ctx context.Context, name string) error {
	if _, err := h.Run(ctx, "sh", "-c", fmt.Sprintf("command -v %s", name)); err != nil {
		return fmt.Errorf("required host command %q not found: %w", name, err)
	}
	return nil
}
