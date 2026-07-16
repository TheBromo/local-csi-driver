// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	cloudInitMountOption = "comment=cloudconfig"
	fstabCommentPrefix   = "# local-csi-nodeprep: "
	fstabBackupSuffix    = ".local-csi-nodeprep.bak"
)

// SourceResolver converts an fstab source such as UUID=... or a symlink into
// its canonical device path.
type SourceResolver func(source string) (string, error)

// FSTabPlan is an immutable, preflight-validated fstab change.
type FSTabPlan struct {
	path       string
	backupPath string
	original   []byte
	backup     []byte
	updated    []byte
}

// FSTabEditor reads and atomically updates the host's fstab.
type FSTabEditor struct {
	hostEtcPath string
}

// NewFSTabEditor constructs an editor rooted at the host /etc mount.
func NewFSTabEditor(hostEtcPath string) *FSTabEditor {
	return &FSTabEditor{hostEtcPath: hostEtcPath}
}

// PlanCloudInitMount verifies that exactly one active /mnt entry exists, that
// cloud-init owns it, and that its evaluated source is resourcePartition.
func (e *FSTabEditor) PlanCloudInitMount(resourcePartition string, resolve SourceResolver) (*FSTabPlan, error) {
	path := filepath.Join(e.hostEtcPath, "fstab")
	original, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read host fstab: %w", err)
	}

	backup, updated, err := planCloudInitMount(original, resourcePartition, resolve)
	if err != nil {
		return nil, err
	}

	return &FSTabPlan{
		path:       path,
		backupPath: path + fstabBackupSuffix,
		original:   original,
		backup:     backup,
		updated:    updated,
	}, nil
}

// Apply writes the backup and atomic replacement only if fstab has not changed
// since PlanCloudInitMount ran.
func (e *FSTabEditor) Apply(plan *FSTabPlan) error {
	if plan == nil || plan.path != filepath.Join(e.hostEtcPath, "fstab") {
		return fmt.Errorf("invalid fstab plan")
	}

	current, err := os.ReadFile(plan.path)
	if err != nil {
		return fmt.Errorf("re-read host fstab: %w", err)
	}
	if !bytes.Equal(current, plan.original) {
		return fmt.Errorf("host fstab changed after preflight")
	}

	info, err := os.Stat(plan.path)
	if err != nil {
		return fmt.Errorf("stat host fstab: %w", err)
	}
	if err := writeBackup(plan.backupPath, plan.backup, info.Mode().Perm()); err != nil {
		return err
	}
	if err := atomicReplace(plan.path, plan.updated, info); err != nil {
		return err
	}
	return nil
}

func planCloudInitMount(data []byte, resourcePartition string, resolve SourceResolver) ([]byte, []byte, error) {
	lines := splitLinesAfter(data)
	type match struct {
		index  int
		marked bool
	}
	matches := make([]match, 0, 1)
	var matchedFields []string

	for index, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\n"))
		if trimmed == "" {
			continue
		}
		marked := strings.HasPrefix(trimmed, fstabCommentPrefix)
		if marked {
			trimmed = strings.TrimPrefix(trimmed, fstabCommentPrefix)
		} else if strings.HasPrefix(trimmed, "#") {
			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) < 4 || decodeFSTabField(fields[1]) != "/mnt" {
			continue
		}
		matches = append(matches, match{index: index, marked: marked})
		matchedFields = fields
	}

	if len(matches) != 1 {
		return nil, nil, fmt.Errorf("expected exactly one active or nodeprep-neutralized /mnt fstab entry, found %d", len(matches))
	}
	if !hasMountOption(matchedFields[3], cloudInitMountOption) {
		return nil, nil, fmt.Errorf("/mnt fstab entry is not marked as cloud-init-owned")
	}

	canonicalSource, err := resolve(decodeFSTabField(matchedFields[0]))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve /mnt fstab source: %w", err)
	}
	if filepath.Clean(canonicalSource) != filepath.Clean(resourcePartition) {
		return nil, nil, fmt.Errorf("/mnt fstab source %s resolves to %s, expected resource partition %s", matchedFields[0], canonicalSource, resourcePartition)
	}

	entry := matches[0]
	index := entry.index
	lineEnding := ""
	line := lines[index]
	if strings.HasSuffix(line, "\n") {
		lineEnding = "\n"
		line = strings.TrimSuffix(line, "\n")
	}

	backupLines := append([]string(nil), lines...)
	updatedLines := append([]string(nil), lines...)
	if entry.marked {
		backupLines[index] = strings.TrimPrefix(strings.TrimSpace(line), fstabCommentPrefix) + lineEnding
	} else {
		updatedLines[index] = fstabCommentPrefix + line + lineEnding
	}
	return []byte(strings.Join(backupLines, "")), []byte(strings.Join(updatedLines, "")), nil
}

func splitLinesAfter(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.SplitAfter(string(data), "\n")
}

func hasMountOption(options, expected string) bool {
	for _, option := range strings.Split(options, ",") {
		if strings.TrimSpace(option) == expected {
			return true
		}
	}
	return false
}

func decodeFSTabField(value string) string {
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\043`, "#",
		`\134`, `\`,
	)
	return replacer.Replace(value)
}

func writeBackup(path string, data []byte, mode fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, fs.ErrExist) {
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read existing fstab backup: %w", readErr)
		}
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("existing fstab backup %s does not match current preflight content", path)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("create fstab backup: %w", err)
	}

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write fstab backup: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync fstab backup: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close fstab backup: %w", err)
	}
	return nil
}

func atomicReplace(path string, data []byte, info fs.FileInfo) error {
	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, ".fstab.local-csi-nodeprep-*")
	if err != nil {
		return fmt.Errorf("create temporary fstab: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()

	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set temporary fstab mode: %w", err)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := temp.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
			_ = temp.Close()
			return fmt.Errorf("set temporary fstab ownership: %w", err)
		}
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary fstab: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary fstab: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary fstab: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace host fstab: %w", err)
	}

	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open fstab directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync fstab directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close fstab directory: %w", err)
	}
	return nil
}

func validateWAAgentConfig(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read WALinuxAgent configuration: %w", err)
	}

	settings := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "ResourceDisk.Format" && key != "ResourceDisk.EnableSwap" {
			continue
		}
		if _, duplicate := settings[key]; duplicate {
			return fmt.Errorf("WALinuxAgent setting %s is configured more than once", key)
		}
		settings[key] = strings.ToLower(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read WALinuxAgent configuration: %w", err)
	}

	for _, key := range []string{"ResourceDisk.Format", "ResourceDisk.EnableSwap"} {
		if value, present := settings[key]; present && value != "n" {
			return fmt.Errorf("WALinuxAgent setting %s must be n when present, found %q", key, value)
		}
	}
	return nil
}
