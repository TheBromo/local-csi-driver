// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"local-csi-driver/internal/csi/core/lvm"
	"local-csi-driver/test/pkg/common"
	"local-csi-driver/test/pkg/utils"
)

// resourceDiskLink is the stable udev link to the Azure ephemeral resource
// disk on the node.
const resourceDiskLink = "/dev/disk/azure/resource"

// LVM on Azure resource disk verifies node preparation
// (diskPreparation.azureResourceDisk) end to end. It requires:
//
//   - an AKS node pool with an Azure temporary/resource disk and no local
//     NVMe, e.g. standard_nv36ads_a10_v5
//     (deploy/parameters/resource-disk-ubuntu.json), and
//   - the chart installed with diskPreparation.azureResourceDisk.enabled=true
//     and allowDestructivePreparation=true.
//
// It is intentionally not labeled "e2e" so the regular e2e/AKS suites never
// run it; use the dedicated make target test-e2e-aks-resource-disk.
var _ = Describe("LVM on Azure resource disk", Label("aks-resource-disk"), Ordered, func() {
	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)
	EnforceDefaultTimeoutsWhenUsingContexts()

	It("should have converted the resource disk to LVM on every node", func(ctx context.Context) {
		for _, pod := range driverNodePods(ctx) {
			By("checking /mnt is unmounted on the host of " + pod)
			_, err := hostExec(ctx, pod, "findmnt", "--mountpoint", "/mnt")
			Expect(err).To(HaveOccurred(), "/mnt should not be mounted on the host of %s", pod)

			By("checking the fstab entry is neutralized on the host of " + pod)
			fstab, err := hostExec(ctx, pod, "cat", "/etc/fstab")
			Expect(err).NotTo(HaveOccurred(), "failed to read fstab on host of %s", pod)
			for _, line := range strings.Split(fstab, "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				fields := strings.Fields(trimmed)
				Expect(len(fields) < 2 || fields[1] != "/mnt").To(BeTrue(),
					"active /mnt entry still present in fstab on host of %s: %s", pod, line)
			}
			Expect(fstab).To(ContainSubstring("local-csi-driver node-prep disabled"),
				"neutralized fstab marker missing on host of %s", pod)

			By("checking the whole resource disk is an LVM2 member on " + pod)
			device, err := hostExec(ctx, pod, "readlink", "-e", resourceDiskLink)
			Expect(err).NotTo(HaveOccurred(), "failed to resolve resource disk on host of %s", pod)
			device = strings.TrimSpace(device)
			fstype, err := hostExec(ctx, pod, "lsblk", "--nodeps", "--noheadings", "--output", "FSTYPE", device)
			Expect(err).NotTo(HaveOccurred(), "failed to read fstype of %s on host of %s", device, pod)
			Expect(strings.TrimSpace(fstype)).To(Equal("LVM2_member"),
				"resource disk %s on host of %s should be an LVM2 member", device, pod)
		}
	})

	It("should expose the volume group on the host and in the driver container", func(ctx context.Context) {
		for _, pod := range driverNodePods(ctx) {
			By("checking the volume group from the host of " + pod)
			hostVgs, err := hostExec(ctx, pod, "vgs", "--noheadings", "--options", "vg_name,vg_tags", lvm.DefaultVolumeGroup)
			Expect(err).NotTo(HaveOccurred(), "volume group not visible from host of %s", pod)
			Expect(hostVgs).To(ContainSubstring(lvm.DefaultVolumeGroupTag), "volume group missing ownership tag on host of %s", pod)

			By("checking the volume group from the driver container of " + pod)
			cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", namespace, pod, "-c", "driver", "--",
				"vgs", "--noheadings", "--options", "vg_name,vg_tags", lvm.DefaultVolumeGroup)
			containerVgs, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "volume group not visible from driver container of %s", pod)
			Expect(containerVgs).To(ContainSubstring(lvm.DefaultVolumeGroupTag), "volume group missing ownership tag in driver container of %s", pod)
		}
	})

	It("should create storageclasses", func(ctx context.Context) {
		for _, fixture := range []string{common.LvmStorageClassFixture, common.LvmStorageClassXfsFixture} {
			cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", fixture)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply storageclass fixture %s", fixture)
		}
	})

	// Regular provisioning, mounting and expansion must behave exactly as on
	// NVMe-backed nodes once the volume group exists.
	lvmExpansionTest("should provision and expand a filesystem PVC on the resource disk", common.LvmPvcAnnotationFixure, common.LvmPodAnnotationFixture, "Filesystem", "/mnt/lcd")
	lvmExpansionTest("should provision and expand a block PVC on the resource disk", common.LvmPvcAnnotationBlockFixture, common.LvmPodAnnotationBlockFixture, "Block", "/dev/lcd")

	It("should not wipe the managed volume group when the driver pod restarts", func(ctx context.Context) {
		pods := driverNodePods(ctx)
		Expect(pods).NotTo(BeEmpty(), "no driver pods found")

		By("recording the volume group UUID before the restart")
		uuidBefore, err := hostExec(ctx, pods[0], "vgs", "--noheadings", "--options", "vg_uuid", lvm.DefaultVolumeGroup)
		Expect(err).NotTo(HaveOccurred(), "failed to read volume group UUID")
		uuidBefore = strings.TrimSpace(uuidBefore)
		Expect(uuidBefore).NotTo(BeEmpty(), "volume group UUID should not be empty")

		By("restarting the driver DaemonSet pods")
		cmd := exec.CommandContext(ctx, "kubectl", "delete", "pod", "-n", namespace,
			"-l", "app.kubernetes.io/component=csi-local-node", "--wait")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "failed to delete driver pods")

		By("waiting for the DaemonSet to become ready again")
		cmd = exec.CommandContext(ctx, "kubectl", "rollout", "status", "--timeout=5m",
			"-n", namespace, "daemonset/"+*helmPrefix+"-node")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "driver DaemonSet did not become ready; node preparation may have failed on restart")

		By("verifying the volume group UUID is unchanged after the restart")
		Eventually(func(g Gomega, ctx context.Context) {
			pods := driverNodePods(ctx)
			g.Expect(pods).NotTo(BeEmpty(), "no driver pods found after restart")
			uuidAfter, err := hostExec(ctx, pods[0], "vgs", "--noheadings", "--options", "vg_uuid", lvm.DefaultVolumeGroup)
			g.Expect(err).NotTo(HaveOccurred(), "failed to read volume group UUID after restart")
			g.Expect(strings.TrimSpace(uuidAfter)).To(Equal(uuidBefore),
				"volume group was recreated during driver restart; the initializer must not wipe a managed VG")
		}).WithContext(ctx).Should(Succeed())
	})

	// The following acceptance scenarios need VM-level operations (az CLI or
	// portal) or extra Azure Disk CSI infrastructure and are exercised
	// manually until the harness can drive them:
	//   - VM reboot: VG, LV and marker data remain visible afterwards.
	//   - VM stop/deallocate or node replacement: the resource disk contents
	//     are discarded by Azure and preparation must reconstruct the VG.
	//   - An unrelated attached Azure Disk CSI volume keeps its signatures,
	//     UUID and contents across installation.
	//   - Negative: preparation must fail without modifying the disk when
	//     kubelet/containerd state or swap lives on the resource disk.
	PIt("should survive a VM reboot with the volume group intact")
	PIt("should reconstruct the volume group after VM deallocation")
	PIt("should not touch attached Azure Disk CSI devices")
	PIt("should fail preparation when the resource disk backs kubelet state or swap")
})

// driverNodePods lists the csi-local-node driver pods.
func driverNodePods(ctx context.Context) []string {
	GinkgoHelper()
	cmd := exec.CommandContext(ctx, "kubectl", "get", "pods", "-n", namespace,
		"-l", "app.kubernetes.io/component=csi-local-node",
		"-o", "jsonpath={.items[*].metadata.name}")
	out, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to list driver pods")
	pods := strings.Fields(out)
	Expect(pods).NotTo(BeEmpty(), "no driver pods found")
	return pods
}

// hostExec runs a command in the host namespaces via the privileged driver
// container (hostPID is enabled on the DaemonSet).
func hostExec(ctx context.Context, pod string, args ...string) (string, error) {
	kubectlArgs := append([]string{
		"exec", "-n", namespace, pod, "-c", "driver", "--",
		"nsenter", "--target", "1", "--mount", "--",
	}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", kubectlArgs...)
	return utils.Run(cmd)
}
