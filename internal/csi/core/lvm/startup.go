// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lvm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kevents "k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"local-csi-driver/internal/pkg/block"
	lvmMgr "local-csi-driver/internal/pkg/lvm"
	"local-csi-driver/internal/pkg/nodeprep"
	"local-csi-driver/internal/pkg/probe"
)

const (
	// Startup diagnostic event reasons.
	noDiskAvailable       = "NoDiskAvailable"
	diskDiscoveryComplete = "DiskDiscoveryComplete"
	volumeGroupReady      = "VolumeGroupReady"
	volumeGroupNotReady   = "VolumeGroupNotReady"
)

// StartupDiagnostic runs a one-time disk availability check when the pod starts
// and emits a Kubernetes event on the pod with the results. It emits a Warning
// event if no disks are available, or a Normal event listing available and
// in-use disks.
type StartupDiagnostic struct {
	probe    probe.Interface
	block    block.Interface
	filter   *probe.Filter
	recorder kevents.EventRecorder
	pod      *corev1.Pod

	// preconfiguredVG, when set, switches the diagnostic to prepared mode:
	// instead of scanning for NVMe disks it verifies the volume group
	// created by node preparation is visible, tagged and backed by the
	// Azure resource disk, and fails driver startup otherwise.
	preconfiguredVG string
	lvm             lvmMgr.Manager
	// resolveLink resolves the Azure resource disk link inside the driver
	// container. Overridable in tests.
	resolveLink func(string) (string, error)
}

// NewStartupDiagnostic creates a new StartupDiagnostic instance.
//
// pod is an ObjectReference to the driver Pod itself (built from downward-API
// env vars at startup, not fetched from the apiserver). It is used as the
// event subject so the diagnostic result is visible via `kubectl describe
// pod`.
func NewStartupDiagnostic(
	p probe.Interface,
	b block.Interface,
	filter *probe.Filter,
	recorder kevents.EventRecorder,
	pod *corev1.Pod,
) *StartupDiagnostic {
	return &StartupDiagnostic{
		probe:       p,
		block:       b,
		filter:      filter,
		recorder:    recorder,
		pod:         pod,
		resolveLink: filepath.EvalSymlinks,
	}
}

// WithPreconfiguredVolumeGroup switches the diagnostic to prepared mode: the
// volume group created by node preparation is verified instead of scanning
// for NVMe disks. resolveLink may be nil to use the default symlink
// resolution; it is injectable for tests.
func (s *StartupDiagnostic) WithPreconfiguredVolumeGroup(vgName string, manager lvmMgr.Manager, resolveLink func(string) (string, error)) *StartupDiagnostic {
	s.preconfiguredVG = vgName
	s.lvm = manager
	if resolveLink != nil {
		s.resolveLink = resolveLink
	}
	return s
}

// Start implements manager.Runnable. It performs the disk availability check
// once at startup and emits a Kubernetes event on the pod with the results.
//
// In prepared mode (preconfiguredVG set) it verifies the preconfigured
// volume group instead and returns an error to fail driver startup when the
// volume group is not usable from the driver container.
func (s *StartupDiagnostic) Start(ctx context.Context) error {
	log := log.FromContext(ctx).WithName("startup-diagnostic")

	if s.preconfiguredVG != "" {
		return s.verifyPreconfiguredVolumeGroup(ctx)
	}

	// Check if there are any available disks.
	devices, err := s.probe.ScanAvailableDevices(ctx)
	if err != nil && !errors.Is(err, probe.ErrNoDevicesFound) {
		log.Error(err, "failed to scan for available devices during startup diagnostic")
		return nil
	}

	// Scan all NVMe devices on the node to build a full picture.
	summary := s.scanNVMeDevices(ctx)

	if devices == nil || len(devices.Devices) == 0 {
		log.Info("no available disks found", "totalNVMeDisks", summary.total, "nonLVM2FormattedDisks", summary.nonLVM2Formatted)
		msg := buildNoDiskMessage(summary)
		s.recorder.Eventf(s.pod, nil, corev1.EventTypeWarning, noDiskAvailable, noDiskAvailable, msg)
		return nil
	}

	log.Info("startup disk discovery complete", "availableDisks", len(devices.Devices))
	msg := buildDiskDiscoveryMessage(devices.Devices, summary)
	s.recorder.Eventf(s.pod, nil, corev1.EventTypeNormal, diskDiscoveryComplete, diskDiscoveryComplete, msg)
	return nil
}

// NeedLeaderElection returns false since the diagnostic should run on every node.
func (s *StartupDiagnostic) NeedLeaderElection() bool {
	return false
}

// verifyPreconfiguredVolumeGroup checks that the volume group prepared on
// the host is visible from the driver container's LVM environment, carries
// the ownership tag, and is backed by the Azure resource disk. It returns
// an error to fail startup when host preparation succeeded but the volume
// group is unusable from the container.
func (s *StartupDiagnostic) verifyPreconfiguredVolumeGroup(ctx context.Context) error {
	log := log.FromContext(ctx).WithName("startup-diagnostic")

	vg, err := s.lvm.GetVolumeGroup(ctx, s.preconfiguredVG)
	if lvmMgr.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to look up preconfigured volume group %s: %w", s.preconfiguredVG, err)
	}

	resourceDisk, linkErr := s.resolveLink(nodeprep.DefaultResourceDiskLink)

	if vg == nil {
		if linkErr != nil {
			// No resource disk on this node and node preparation was
			// configured as optional: report and continue without failing.
			msg := fmt.Sprintf("Preconfigured volume group %s not found and no Azure resource disk is present on this node. "+
				"Volume provisioning will not be possible on this node.", s.preconfiguredVG)
			log.Info("preconfigured volume group and resource disk not found", "vg", s.preconfiguredVG)
			s.recorder.Eventf(s.pod, nil, corev1.EventTypeWarning, volumeGroupNotReady, volumeGroupNotReady, msg)
			return nil
		}
		s.recorder.Eventf(s.pod, nil, corev1.EventTypeWarning, volumeGroupNotReady, volumeGroupNotReady,
			fmt.Sprintf("Preconfigured volume group %s is not visible from the driver container although the resource disk %s exists.", s.preconfiguredVG, resourceDisk))
		return fmt.Errorf("preconfigured volume group %s is not visible from the driver container's LVM environment (resource disk %s exists); node preparation and driver disagree", s.preconfiguredVG, resourceDisk)
	}

	if !hasVolumeGroupTag(vg.Tags, DefaultVolumeGroupTag) {
		s.recorder.Eventf(s.pod, nil, corev1.EventTypeWarning, volumeGroupNotReady, volumeGroupNotReady,
			fmt.Sprintf("Preconfigured volume group %s does not carry the %s ownership tag (tags: %q).", s.preconfiguredVG, DefaultVolumeGroupTag, vg.Tags))
		return fmt.Errorf("preconfigured volume group %s does not carry the %s ownership tag (tags: %q)", s.preconfiguredVG, DefaultVolumeGroupTag, vg.Tags)
	}

	pvs, err := s.lvm.ListPhysicalVolumes(ctx, &lvmMgr.ListPVOptions{Select: "vg_name=" + s.preconfiguredVG})
	if err != nil {
		return fmt.Errorf("failed to list physical volumes of preconfigured volume group %s: %w", s.preconfiguredVG, err)
	}
	pvNames := make([]string, 0, len(pvs))
	for _, pv := range pvs {
		pvNames = append(pvNames, pv.Name)
	}

	if linkErr == nil {
		member := false
		for _, name := range pvNames {
			if name == resourceDisk {
				member = true
				break
			}
		}
		if !member {
			s.recorder.Eventf(s.pod, nil, corev1.EventTypeWarning, volumeGroupNotReady, volumeGroupNotReady,
				fmt.Sprintf("Preconfigured volume group %s does not include the Azure resource disk %s (members: %s).", s.preconfiguredVG, resourceDisk, strings.Join(pvNames, ", ")))
			return fmt.Errorf("preconfigured volume group %s does not include the Azure resource disk %s (members: %s)", s.preconfiguredVG, resourceDisk, strings.Join(pvNames, ", "))
		}
	}

	msg := fmt.Sprintf("Volume group %s is ready: %d physical volume(s) (%s), %s free of %s.",
		s.preconfiguredVG, len(pvNames), strings.Join(pvNames, ", "),
		formatBytes(int64(vg.Free)), formatBytes(int64(vg.Size)))
	log.Info("preconfigured volume group verified", "vg", s.preconfiguredVG, "pvs", pvNames, "free", int64(vg.Free), "size", int64(vg.Size))
	s.recorder.Eventf(s.pod, nil, corev1.EventTypeNormal, volumeGroupReady, volumeGroupReady, msg)
	return nil
}

// hasVolumeGroupTag reports whether the comma-separated LVM tag list
// contains tag.
func hasVolumeGroupTag(tags, tag string) bool {
	for _, t := range strings.Split(tags, ",") {
		if strings.TrimSpace(t) == tag {
			return true
		}
	}
	return false
}

// deviceSummary holds the results of scanning all NVMe devices on the node.
type deviceSummary struct {
	total            int
	nonLVM2Formatted int
	available        []block.Device
	inUse            []block.Device
}

// scanNVMeDevices scans all block devices and categorizes NVMe devices
// matching the filter. Returns a summary with device lists.
func (s *StartupDiagnostic) scanNVMeDevices(ctx context.Context) deviceSummary {
	log := log.FromContext(ctx).WithName("startup-diagnostic")

	allDevices, err := s.block.GetDevices(ctx)
	if err != nil {
		log.Error(err, "failed to list all block devices for diagnostic context")
		return deviceSummary{}
	}

	var summary deviceSummary
	for _, d := range allDevices.Devices {
		if !s.filter.Match(d) {
			continue
		}
		summary.total++

		isFormatted, err := s.block.IsFormatted(d.Path)
		if err != nil {
			log.Error(err, "failed to check device format status", "device", d.Path)
			continue
		}
		if !isFormatted {
			log.V(1).Info("NVMe device found (unformatted)", "path", d.Path, "model", d.Model, "size", d.Size)
			summary.available = append(summary.available, d)
			continue
		}

		isLVM2, err := s.block.IsLVM2(d.Path)
		if err != nil {
			log.Error(err, "failed to check device LVM2 status", "device", d.Path)
			continue
		}
		if isLVM2 {
			log.V(1).Info("NVMe device found (LVM2, part of a volume group)", "path", d.Path, "model", d.Model, "size", d.Size)
			summary.available = append(summary.available, d)
			continue
		}

		summary.nonLVM2Formatted++
		log.V(1).Info("NVMe device found (formatted, non-LVM2)", "path", d.Path, "model", d.Model, "size", d.Size)
		summary.inUse = append(summary.inUse, d)
	}

	return summary
}

// formatDeviceList formats a list of devices into a human-readable string.
func formatDeviceList(devices []block.Device) string {
	names := make([]string, len(devices))
	for i, d := range devices {
		names[i] = fmt.Sprintf("%s (%s)", d.Path, formatBytes(d.Size))
	}
	return strings.Join(names, ", ")
}

// formatBytes formats bytes into a human-readable string.
func formatBytes(b int64) string {
	const gi = 1024 * 1024 * 1024
	if b >= gi {
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gi))
	}
	const mi = 1024 * 1024
	return fmt.Sprintf("%.1f MiB", float64(b)/float64(mi))
}

// buildDiskDiscoveryMessage constructs a Normal event message listing
// available and in-use devices.
func buildDiskDiscoveryMessage(available []block.Device, summary deviceSummary) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Disk discovery complete: found %d available disk(s) for volume group creation", len(available))

	sb.WriteString(". Available: ")
	sb.WriteString(formatDeviceList(available))

	if len(summary.inUse) > 0 {
		sb.WriteString(". In use (non-LVM formatted): ")
		sb.WriteString(formatDeviceList(summary.inUse))
	}

	return sb.String()
}

// buildNoDiskMessage constructs a Warning event message when no disks are
// available, with diagnostic context and remediation advice.
func buildNoDiskMessage(summary deviceSummary) string {
	if summary.total == 0 {
		return "No NVMe disks matching the expected model (Microsoft NVMe Direct Disk) " +
			"were found on this node. This can happen when the node pool uses a VM SKU " +
			"with ephemeral OS disk enabled, which consumes the NVMe disk for the OS. " +
			"Consider using a VM SKU with additional NVMe disks, or disable " +
			"ephemeral OS disk on the node pool."
	}

	if summary.nonLVM2Formatted == summary.total {
		return fmt.Sprintf(
			"No available disks for volume group creation. Found %d NVMe disk(s) "+
				"on this node, but all are already formatted with a non-LVM filesystem: %s. "+
				"Consider using a VM SKU with additional NVMe disks.",
			summary.total, formatDeviceList(summary.inUse),
		)
	}

	return fmt.Sprintf(
		"No available disks for volume group creation. Found %d NVMe disk(s) "+
			"on this node (%d formatted with a non-LVM filesystem, %d unformatted or already "+
			"in a volume group), but none are newly available for volume group creation.",
		summary.total, summary.nonLVM2Formatted, summary.total-summary.nonLVM2Formatted,
	)
}
