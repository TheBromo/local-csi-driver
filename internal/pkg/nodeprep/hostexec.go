// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const defaultLockRetryInterval = 100 * time.Millisecond

// Command is a host command invocation. Name and Args are kept separate so
// callers never need to construct shell command strings.
type Command struct {
	Name string
	Args []string
}

// Output contains the bytes written by a host command.
type Output struct {
	Stdout []byte
	Stderr []byte
}

// CommandRunner executes commands in the host namespaces.
//
//go:generate mockgen -copyright_file ../../../hack/mockgen_copyright.txt -destination=mock_hostexec.go -mock_names=CommandRunner=MockCommandRunner -package=nodeprep -source=hostexec.go CommandRunner
type CommandRunner interface {
	Run(ctx context.Context, command Command) (Output, error)
}

// CommandError describes a host command that exited unsuccessfully.
type CommandError struct {
	Command  Command
	Stderr   string
	ExitCode int
	Err      error
}

func (e *CommandError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("host command %q failed: %v", e.Command.Name, e.Err)
	}
	return fmt.Sprintf("host command %q failed: %v: %s", e.Command.Name, e.Err, e.Stderr)
}

// Unwrap preserves cancellation and process errors for errors.Is/errors.As.
func (e *CommandError) Unwrap() error {
	return e.Err
}

// IsExitCode reports whether err is a CommandError with the given exit code.
func IsExitCode(err error, code int) bool {
	var commandErr *CommandError
	return errors.As(err, &commandErr) && commandErr.ExitCode == code
}

// HostRunner executes every command through nsenter against host PID 1.
type HostRunner struct {
	nsenterPath    string
	commandContext func(context.Context, string, ...string) *exec.Cmd
}

var _ CommandRunner = (*HostRunner)(nil)

// NewHostRunner returns a runner that enters the host mount, UTS, IPC,
// network, and PID namespaces for every command.
func NewHostRunner() (*HostRunner, error) {
	nsenterPath, err := exec.LookPath("nsenter")
	if err != nil {
		return nil, fmt.Errorf("find nsenter: %w", err)
	}

	return &HostRunner{
		nsenterPath:    nsenterPath,
		commandContext: exec.CommandContext,
	}, nil
}

// Run invokes command through nsenter --target 1.
func (r *HostRunner) Run(ctx context.Context, command Command) (Output, error) {
	if command.Name == "" {
		return Output{}, fmt.Errorf("host command name cannot be empty")
	}

	args := []string{
		"--target", "1",
		"--root",
		"--wd",
		"--mount",
		"--uts",
		"--ipc",
		"--net",
		"--pid",
		"--",
		command.Name,
	}
	args = append(args, command.Args...)

	cmd := r.commandContext(ctx, r.nsenterPath, args...)
	// The driver image disables LVM udev integration, but node preparation
	// explicitly relies on host udev settling and must not inherit that setting.
	cmd.Env = withoutEnvironmentVariable(os.Environ(), "DM_DISABLE_UDEV")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := Output{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return output, nil
	}

	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	return output, &CommandError{
		Command:  command,
		Stderr:   strings.TrimSpace(stderr.String()),
		ExitCode: exitCode,
		Err:      err,
	}
}

func withoutEnvironmentVariable(environment []string, name string) []string {
	prefix := name + "="
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// Locker serializes node preparation across all pods on a host.
type Locker interface {
	Acquire(ctx context.Context) (Lock, error)
}

// Lock is held until Release is called.
type Lock interface {
	Release() error
}

// FileLocker acquires an advisory lock on a file visible through host PID 1's
// root. The default path is under /proc/1/root/run/lock, so no extra host /run
// mount is required by the init container.
type FileLocker struct {
	path          string
	retryInterval time.Duration
}

// NewFileLocker constructs a host-wide file locker.
func NewFileLocker(path string) *FileLocker {
	return &FileLocker{path: path, retryInterval: defaultLockRetryInterval}
}

// Acquire waits until the lock is held or ctx is cancelled.
func (l *FileLocker) Acquire(ctx context.Context) (Lock, error) {
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open host preparation lock %s: %w", l.path, err)
	}

	ticker := time.NewTicker(l.retryInterval)
	defer ticker.Stop()

	for {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return &fileLock{file: file}, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire host preparation lock %s: %w", l.path, err)
		}

		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("wait for host preparation lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

type fileLock struct {
	file *os.File
}

func (l *fileLock) Release() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if err := errors.Join(unlockErr, closeErr); err != nil {
		return fmt.Errorf("release host preparation lock: %w", err)
	}
	return nil
}
