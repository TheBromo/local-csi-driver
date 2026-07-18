// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fstabNeutralizedPrefix marks fstab lines commented out by node
// preparation so operators can identify (and restore) them.
const fstabNeutralizedPrefix = "# local-csi-driver node-prep disabled: "

// fstabEntry is a single active (non-comment) fstab line.
type fstabEntry struct {
	// Spec is the device specification (first field), e.g.
	// /dev/disk/cloud/azure_resource-part1.
	Spec string
	// File is the mount target (second field), e.g. /mnt.
	File string
	// MntOps are the mount options (fourth field).
	MntOps string
	// LineNo is the zero-based line index in the file.
	LineNo int
}

// parseFstab returns the active entries of an fstab file.
func parseFstab(content string) []fstabEntry {
	lines := strings.Split(content, "\n")
	entries := make([]fstabEntry, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 4 {
			continue
		}
		entries = append(entries, fstabEntry{
			Spec:   fields[0],
			File:   fields[1],
			MntOps: fields[3],
			LineNo: i,
		})
	}
	return entries
}

// isCloudInitManaged reports whether the entry carries the markers
// cloud-init's mounts module writes for the Azure ephemeral resource disk.
func (e fstabEntry) isCloudInitManaged() bool {
	return strings.Contains(e.MntOps, "comment=cloudconfig") ||
		strings.Contains(e.MntOps, "x-systemd.after=cloud-init")
}

// findMntEntry locates the single active /mnt entry in the fstab content.
//
// It fails closed when /mnt is configured in a way node preparation does not
// recognize: multiple /mnt entries, or an entry without the cloud-init
// markers. A missing entry returns (nil, nil).
func findMntEntry(content string) (*fstabEntry, error) {
	var matches []fstabEntry
	for _, e := range parseFstab(content) {
		if filepath.Clean(e.File) == mntTarget {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		if !matches[0].isCloudInitManaged() {
			return nil, fmt.Errorf("fstab entry for /mnt (%s) is not cloud-init managed; refusing to modify a custom /mnt configuration", matches[0].Spec)
		}
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("found %d fstab entries for /mnt; refusing to modify an ambiguous /mnt configuration", len(matches))
	}
}

// neutralizeFstabLine comments out exactly the given line, prefixing it with
// the node-prep marker, and returns the new content.
func neutralizeFstabLine(content string, lineNo int) (string, error) {
	lines := strings.Split(content, "\n")
	if lineNo < 0 || lineNo >= len(lines) {
		return "", fmt.Errorf("fstab line %d out of range", lineNo)
	}
	lines[lineNo] = fstabNeutralizedPrefix + lines[lineNo]
	return strings.Join(lines, "\n"), nil
}

// writeFileAtomic atomically replaces path with data by writing a temporary
// file in the same directory and renaming it over the target.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".nodeprep-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup, no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // write error takes precedence
		return fmt.Errorf("failed to write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close() //nolint:errcheck // chmod error takes precedence
		return fmt.Errorf("failed to chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck // sync error takes precedence
		return fmt.Errorf("failed to sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}
