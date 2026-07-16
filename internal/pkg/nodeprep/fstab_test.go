// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFSTabEditorCommentsOnlyCloudInitResourceMount(t *testing.T) {
	t.Parallel()

	hostEtc := t.TempDir()
	fstabPath := filepath.Join(hostEtc, "fstab")
	original := strings.Join([]string{
		"UUID=root / ext4 defaults 0 1",
		"/dev/disk/cloud/azure_resource-part1 /mnt auto defaults,nofail,x-systemd.after=cloud-init-network.service,comment=cloudconfig 0 2",
		"server:/data /srv nfs defaults 0 0",
		"",
	}, "\n")
	if err := os.WriteFile(fstabPath, []byte(original), 0644); err != nil {
		t.Fatalf("write fstab fixture: %v", err)
	}

	editor := NewFSTabEditor(hostEtc)
	plan, err := editor.PlanCloudInitMount(fixtureResourcePartition, func(source string) (string, error) {
		if source != "/dev/disk/cloud/azure_resource-part1" {
			t.Fatalf("resolver source = %q", source)
		}
		return fixtureResourcePartition, nil
	})
	if err != nil {
		t.Fatalf("PlanCloudInitMount() error = %v", err)
	}
	if err := editor.Apply(plan); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	updated, err := os.ReadFile(fstabPath)
	if err != nil {
		t.Fatalf("read updated fstab: %v", err)
	}
	wantComment := fstabCommentPrefix + "/dev/disk/cloud/azure_resource-part1 /mnt auto defaults,nofail,x-systemd.after=cloud-init-network.service,comment=cloudconfig 0 2"
	if !strings.Contains(string(updated), wantComment) {
		t.Fatalf("updated fstab = %q, want cloud-init line commented", updated)
	}
	if !strings.Contains(string(updated), "UUID=root / ext4 defaults 0 1") || !strings.Contains(string(updated), "server:/data /srv nfs defaults 0 0") {
		t.Fatalf("updated fstab changed unrelated entries: %q", updated)
	}

	backup, err := os.ReadFile(fstabPath + fstabBackupSuffix)
	if err != nil {
		t.Fatalf("read fstab backup: %v", err)
	}
	if string(backup) != original {
		t.Fatalf("backup = %q, want original %q", backup, original)
	}
}

func TestFSTabEditorRejectsCustomOrAmbiguousMounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		contents  string
		wantError string
	}{
		{
			name:      "custom entry",
			contents:  "/dev/sdb1 /mnt ext4 defaults 0 2\n",
			wantError: "not marked as cloud-init-owned",
		},
		{
			name: "ambiguous entries",
			contents: strings.Join([]string{
				"/dev/sdb1 /mnt auto defaults,comment=cloudconfig 0 2",
				"UUID=duplicate /mnt auto defaults,comment=cloudconfig 0 2",
				"",
			}, "\n"),
			wantError: "found 2",
		},
		{
			name:      "wrong source",
			contents:  "/dev/sdc1 /mnt auto defaults,comment=cloudconfig 0 2\n",
			wantError: "expected resource partition",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hostEtc := t.TempDir()
			if err := os.WriteFile(filepath.Join(hostEtc, "fstab"), []byte(test.contents), 0644); err != nil {
				t.Fatalf("write fstab fixture: %v", err)
			}
			editor := NewFSTabEditor(hostEtc)
			_, err := editor.PlanCloudInitMount(fixtureResourcePartition, func(string) (string, error) { return "/dev/sdc1", nil })
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("PlanCloudInitMount() error = %v, want containing %q", err, test.wantError)
			}
			if _, statErr := os.Stat(filepath.Join(hostEtc, "fstab") + fstabBackupSuffix); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("backup exists after rejected plan: %v", statErr)
			}
		})
	}
}

func TestFSTabEditorRejectsConcurrentChange(t *testing.T) {
	t.Parallel()

	hostEtc := t.TempDir()
	path := filepath.Join(hostEtc, "fstab")
	original := "/dev/sdb1 /mnt auto defaults,comment=cloudconfig 0 2\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatalf("write fstab fixture: %v", err)
	}
	editor := NewFSTabEditor(hostEtc)
	plan, err := editor.PlanCloudInitMount(fixtureResourcePartition, func(string) (string, error) { return fixtureResourcePartition, nil })
	if err != nil {
		t.Fatalf("PlanCloudInitMount() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(original+"# changed\n"), 0644); err != nil {
		t.Fatalf("change fstab fixture: %v", err)
	}
	if err := editor.Apply(plan); err == nil || !strings.Contains(err.Error(), "changed after preflight") {
		t.Fatalf("Apply() error = %v, want concurrent change rejection", err)
	}
}

func TestFSTabEditorResumesItsNeutralizedEntry(t *testing.T) {
	t.Parallel()

	hostEtc := t.TempDir()
	path := filepath.Join(hostEtc, "fstab")
	original := "/dev/sdb1 /mnt auto defaults,comment=cloudconfig 0 2\n"
	neutralized := fstabCommentPrefix + original
	if err := os.WriteFile(path, []byte(neutralized), 0644); err != nil {
		t.Fatalf("write neutralized fstab fixture: %v", err)
	}
	if err := os.WriteFile(path+fstabBackupSuffix, []byte(original), 0644); err != nil {
		t.Fatalf("write fstab backup fixture: %v", err)
	}

	editor := NewFSTabEditor(hostEtc)
	plan, err := editor.PlanCloudInitMount(fixtureResourcePartition, func(string) (string, error) { return fixtureResourcePartition, nil })
	if err != nil {
		t.Fatalf("PlanCloudInitMount() error = %v", err)
	}
	if err := editor.Apply(plan); err != nil {
		t.Fatalf("Apply(resume) error = %v", err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resumed fstab: %v", err)
	}
	if string(updated) != neutralized {
		t.Fatalf("resumed fstab = %q, want %q", updated, neutralized)
	}
}

func TestValidateWAAgentConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		contents  string
		wantError string
	}{
		{
			name: "Ubuntu defaults",
			contents: strings.Join([]string{
				"ResourceDisk.Format=n",
				"ResourceDisk.EnableSwap=n",
				"",
			}, "\n"),
		},
		{name: "settings absent", contents: "Provisioning.Agent=auto\n"},
		{name: "format enabled", contents: "ResourceDisk.Format=y\n", wantError: "must be n"},
		{name: "swap enabled", contents: "ResourceDisk.EnableSwap=y\n", wantError: "must be n"},
		{
			name:      "ambiguous duplicate",
			contents:  "ResourceDisk.Format=n\nResourceDisk.Format=n\n",
			wantError: "more than once",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "waagent.conf")
			if err := os.WriteFile(path, []byte(test.contents), 0644); err != nil {
				t.Fatalf("write waagent fixture: %v", err)
			}
			err := validateWAAgentConfig(path)
			if test.wantError == "" && err != nil {
				t.Fatalf("validateWAAgentConfig() error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("validateWAAgentConfig() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}
