// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeprep

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// vgInfo is the fake host's record of a volume group.
type vgInfo struct {
	tags string
}

// findmntCmd is the host findmnt command name.
const findmntCmd = "findmnt"

// fakeHost emulates the host-side commands node preparation runs, keeping
// just enough state (device tree, LVM metadata, mounts) to exercise the
// full state machine without real disks.
type fakeHost struct {
	t  *testing.T
	mu sync.Mutex

	// calls records every command line in order.
	calls []string

	// linkTargets maps a symlink to its successive resolutions; the last
	// entry repeats. An empty string target means the link is missing.
	linkTargets map[string][]string
	linkCount   map[string]int

	// runtimePaths overrides readlink -f resolution for host paths.
	runtimePaths map[string]string

	// trees maps a whole-disk device path to its lsblk tree.
	trees map[string]*Device

	// pvs maps a PV device path to its volume group ("" = no VG).
	pvs map[string]string
	// vgs maps a volume group name to its metadata.
	vgs map[string]*vgInfo

	// otherMounts are extra findmnt --list targets beyond the tree mounts.
	otherMounts []string

	cloudInitOutput string
	cloudInitErr    error
	cloudInitHang   bool

	missingCommands map[string]bool

	// failOn fails any command whose command line contains the key.
	failOn map[string]error

	lvmDevicesOut string
}

func newFakeHost(t *testing.T) *fakeHost {
	return &fakeHost{
		t:               t,
		linkTargets:     map[string][]string{},
		linkCount:       map[string]int{},
		runtimePaths:    map[string]string{},
		trees:           map[string]*Device{},
		pvs:             map[string]string{},
		vgs:             map[string]*vgInfo{},
		missingCommands: map[string]bool{},
		failOn:          map[string]error{},
	}
}

var _ Runner = &fakeHost{}

func (f *fakeHost) LookPath(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "lookpath "+name)
	if f.missingCommands[name] {
		return fmt.Errorf("required host command %q not found", name)
	}
	return nil
}

func (f *fakeHost) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := name
	if len(args) > 0 {
		cmd += " " + strings.Join(args, " ")
	}
	f.calls = append(f.calls, cmd)

	for substr, err := range f.failOn {
		if strings.Contains(cmd, substr) {
			return nil, err
		}
	}

	out, err := f.dispatch(ctx, name, args, cmd)
	return []byte(out), err
}

//nolint:gocyclo // test emulator dispatch table
func (f *fakeHost) dispatch(ctx context.Context, name string, args []string, cmd string) (string, error) {
	switch {
	case name == "readlink" && len(args) == 2 && args[0] == "-e":
		return f.resolveLink(args[1])

	case name == "readlink" && len(args) == 2 && args[0] == "-f":
		if resolved, ok := f.runtimePaths[args[1]]; ok {
			return resolved + "\n", nil
		}
		return args[1] + "\n", nil

	case name == "lsblk":
		device := args[len(args)-1]
		tree, ok := f.trees[device]
		if !ok {
			return "", fmt.Errorf("lsblk: %s: not a block device", device)
		}
		out, err := json.Marshal(deviceTree{BlockDevices: []Device{*tree}})
		return string(out), err

	case name == "cloud-init":
		if f.cloudInitHang {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return f.cloudInitOutput, f.cloudInitErr

	case name == "udevadm":
		return "", nil

	case name == "sh" && len(args) == 2 && args[0] == "-c":
		cmdName := strings.TrimPrefix(args[1], "command -v ")
		if f.missingCommands[cmdName] {
			return "", fmt.Errorf("command %s not found", cmdName)
		}
		return "/usr/sbin/" + cmdName + "\n", nil

	case name == "pvs":
		return f.pvsReport(cmd)

	case name == "vgs":
		return f.vgsReport(cmd)

	case name == "pvcreate" && len(args) == 1:
		f.pvs[args[0]] = ""
		return "", nil

	case name == "vgcreate":
		// vgcreate --addtag <tag> <vg> <device>
		if len(args) != 4 || args[0] != "--addtag" {
			return "", fmt.Errorf("unexpected vgcreate invocation: %s", cmd)
		}
		f.vgs[args[2]] = &vgInfo{tags: args[1]}
		f.pvs[args[3]] = args[2]
		return "", nil

	case name == "vgchange":
		vg := args[len(args)-1]
		if _, ok := f.vgs[vg]; !ok {
			return "", fmt.Errorf("volume group %s not found", vg)
		}
		return "", nil

	case name == "pvscan" || name == "vgscan":
		return "", nil

	case name == "systemctl" && len(args) == 2 && args[0] == "stop":
		f.unmountMnt()
		return "", nil

	case name == "systemctl" && len(args) == 1 && args[0] == "daemon-reload":
		return "", nil

	case name == "umount":
		f.unmountMnt()
		return "", nil

	case name == findmntCmd && len(args) == 2 && args[0] == "--mountpoint":
		if f.mntMounted(args[1]) {
			return args[1] + "\n", nil
		}
		return "", fmt.Errorf("findmnt: %s: not mounted", args[1])

	case name == findmntCmd && args[0] == "--list":
		targets := append([]string{}, f.otherMounts...)
		for _, tree := range f.trees {
			for _, m := range tree.mounts() {
				targets = append(targets, m.Target)
			}
		}
		return strings.Join(targets, "\n") + "\n", nil

	case name == findmntCmd && args[0] == "--verify":
		return "", nil

	case name == "wipefs" && len(args) == 3 && args[1] == "--force":
		tree, ok := f.trees[args[2]]
		if !ok {
			return "", fmt.Errorf("wipefs: %s: not a block device", args[2])
		}
		tree.Children = nil
		tree.FSType = ""
		return "", nil

	case name == "wipefs" && len(args) == 2 && args[0] == "--all":
		for _, tree := range f.trees {
			for i := range tree.Children {
				if tree.Children[i].Path == args[1] {
					tree.Children[i].FSType = ""
					return "", nil
				}
			}
		}
		return "", fmt.Errorf("wipefs: %s: not found", args[1])

	case name == "blockdev":
		return "", nil

	case name == "lvmdevices" && len(args) == 0:
		return f.lvmDevicesOut, nil

	case name == "lvmdevices" && len(args) == 2 && args[0] == "--adddev":
		f.lvmDevicesOut += " DEVNAME=" + args[1] + "\n"
		return "", nil

	default:
		return "", fmt.Errorf("fakeHost: unexpected command %q", cmd)
	}
}

// resolveLink emulates readlink -e with successive resolutions per link.
func (f *fakeHost) resolveLink(link string) (string, error) {
	if targets, ok := f.linkTargets[link]; ok {
		idx := f.linkCount[link]
		if idx >= len(targets) {
			idx = len(targets) - 1
		}
		f.linkCount[link]++
		if targets[idx] == "" {
			return "", fmt.Errorf("readlink: %s: no such file or directory", link)
		}
		return targets[idx] + "\n", nil
	}
	// Device paths resolve to themselves when they exist in a tree.
	for _, tree := range f.trees {
		found := ""
		tree.walk(func(d *Device) {
			if d.Path == link {
				found = link
			}
		})
		if found != "" {
			return found + "\n", nil
		}
	}
	return "", fmt.Errorf("readlink: %s: no such file or directory", link)
}

// pvsReport emulates pvs --reportformat json --select ...
func (f *fakeHost) pvsReport(cmd string) (string, error) {
	selector := selectorFrom(cmd)
	var rows []map[string]string
	switch {
	case strings.HasPrefix(selector, "pv_name="):
		device := strings.TrimPrefix(selector, "pv_name=")
		if vg, ok := f.pvs[device]; ok {
			rows = append(rows, map[string]string{"pv_name": device, "vg_name": vg})
		}
	case strings.HasPrefix(selector, "vg_name="):
		vg := strings.TrimPrefix(selector, "vg_name=")
		for device, pvVG := range f.pvs {
			if pvVG == vg {
				rows = append(rows, map[string]string{"pv_name": device, "vg_name": vg})
			}
		}
	default:
		return "", fmt.Errorf("fakeHost: unexpected pvs selector %q", selector)
	}
	return marshalReport("pv", rows)
}

// vgsReport emulates vgs --reportformat json --select ...
func (f *fakeHost) vgsReport(cmd string) (string, error) {
	selector := selectorFrom(cmd)
	if !strings.HasPrefix(selector, "vg_name=") {
		return "", fmt.Errorf("fakeHost: unexpected vgs selector %q", selector)
	}
	vg := strings.TrimPrefix(selector, "vg_name=")
	var rows []map[string]string
	if info, ok := f.vgs[vg]; ok {
		rows = append(rows, map[string]string{"vg_name": vg, "vg_tags": info.tags})
	}
	return marshalReport("vg", rows)
}

func selectorFrom(cmd string) string {
	fields := strings.Fields(cmd)
	for i, field := range fields {
		if field == "--select" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func marshalReport(section string, rows []map[string]string) (string, error) {
	if rows == nil {
		rows = []map[string]string{}
	}
	out, err := json.Marshal(map[string][]map[string][]map[string]string{
		"report": {{section: rows}},
	})
	return string(out), err
}

// mntMounted reports whether any tree device is mounted at target.
func (f *fakeHost) mntMounted(target string) bool {
	for _, tree := range f.trees {
		for _, m := range tree.mounts() {
			if m.Target == target {
				return true
			}
		}
	}
	return false
}

// unmountMnt removes the /mnt mountpoint from all tree devices.
func (f *fakeHost) unmountMnt() {
	for _, tree := range f.trees {
		tree.walk(func(d *Device) {
			if d.Mountpoint == mntTarget {
				d.Mountpoint = ""
			}
			var kept []string
			for _, m := range d.Mountpoints {
				if m != mntTarget {
					kept = append(kept, m)
				}
			}
			d.Mountpoints = kept
		})
	}
}

// callsMatching returns recorded command invocations containing the
// substring. LookPath probes are not command invocations and are excluded.
func (f *fakeHost) callsMatching(substr string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "lookpath ") {
			continue
		}
		if strings.Contains(c, substr) {
			matched = append(matched, c)
		}
	}
	return matched
}
