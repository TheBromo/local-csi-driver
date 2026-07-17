// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFstab(t *testing.T) {
	t.Parallel()
	content := `# comment
UUID=1111 / ext4 defaults 0 1

   # indented comment
/dev/sdb1	/mnt	auto	defaults,nofail,comment=cloudconfig	0	2
short line
`
	entries := parseFstab(content)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].Spec != "UUID=1111" || entries[0].File != "/" {
		t.Errorf("unexpected first entry: %+v", entries[0])
	}
	if entries[1].Spec != "/dev/sdb1" || entries[1].File != "/mnt" || entries[1].LineNo != 4 {
		t.Errorf("unexpected second entry: %+v", entries[1])
	}
}

func TestFindMntEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		content   string
		expectNil bool
		expectErr string
		spec      string
	}{
		{
			name:      "no mnt entry",
			content:   "UUID=1111 / ext4 defaults 0 1\n",
			expectNil: true,
		},
		{
			name:    "cloud-init comment marker",
			content: "/dev/sdb1 /mnt auto defaults,nofail,comment=cloudconfig 0 2\n",
			spec:    "/dev/sdb1",
		},
		{
			name:    "cloud-init systemd marker",
			content: "/dev/sdb1 /mnt auto defaults,x-systemd.after=cloud-init.service 0 2\n",
			spec:    "/dev/sdb1",
		},
		{
			name:      "custom entry rejected",
			content:   "/dev/sdb1 /mnt ext4 defaults 0 2\n",
			expectErr: "not cloud-init managed",
		},
		{
			name: "ambiguous entries rejected",
			content: "/dev/sdb1 /mnt auto comment=cloudconfig 0 2\n" +
				"/dev/sdc1 /mnt auto comment=cloudconfig 0 2\n",
			expectErr: "ambiguous",
		},
		{
			name:      "commented entry ignored",
			content:   "# /dev/sdb1 /mnt auto comment=cloudconfig 0 2\n",
			expectNil: true,
		},
		{
			name:    "trailing slash normalized",
			content: "/dev/sdb1 /mnt/ auto comment=cloudconfig 0 2\n",
			spec:    "/dev/sdb1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry, err := findMntEntry(tc.content)
			if tc.expectErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.expectErr) {
					t.Fatalf("expected error containing %q, got: %v", tc.expectErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.expectNil {
				if entry != nil {
					t.Fatalf("expected no entry, got: %+v", entry)
				}
				return
			}
			if entry == nil || entry.Spec != tc.spec {
				t.Fatalf("expected entry with spec %q, got: %+v", tc.spec, entry)
			}
		})
	}
}

func TestNeutralizeFstabLine(t *testing.T) {
	t.Parallel()
	content := "UUID=1111 / ext4 defaults 0 1\n/dev/sdb1 /mnt auto comment=cloudconfig 0 2\nUUID=2222 /boot vfat defaults 0 2\n"
	updated, err := neutralizeFstabLine(content, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(updated, "\n")
	if lines[0] != "UUID=1111 / ext4 defaults 0 1" || lines[2] != "UUID=2222 /boot vfat defaults 0 2" {
		t.Errorf("unrelated lines modified: %q", updated)
	}
	if lines[1] != fstabNeutralizedPrefix+"/dev/sdb1 /mnt auto comment=cloudconfig 0 2" {
		t.Errorf("target line not neutralized: %q", lines[1])
	}
	// The neutralized line must no longer parse as an active entry.
	if entries := parseFstab(updated); len(entries) != 2 {
		t.Errorf("expected 2 active entries after neutralizing, got %d", len(entries))
	}

	if _, err := neutralizeFstabLine(content, 17); err == nil {
		t.Error("expected out of range error")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "fstab")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0644); err != nil {
		t.Fatalf("writeFileAtomic failed: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(content) != "new" {
		t.Errorf("expected %q, got %q", "new", content)
	}
	// No temp files may be left behind.
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("expected only the target file in dir, got %d files", len(files))
	}
}
