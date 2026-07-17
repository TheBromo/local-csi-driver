// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// local-csi-nodeprep prepares the Azure ephemeral resource disk of a node
// for use as an LVM volume group backing local-csi-driver volumes.
//
// It is intended to run as a privileged init container (with hostPID and
// the host /dev, /etc and /run mounts) before the CSI driver starts. Host
// commands are executed through nsenter into PID 1's namespaces.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/klog/v2/textlogger"
	utilexec "k8s.io/utils/exec"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"local-csi-driver/internal/pkg/nodeprep"
	"local-csi-driver/internal/pkg/version"
)

// terminationMessagePath is the path to the termination message file for
// the Kubernetes pod. This file is used to store the last error message.
const terminationMessagePath = "/tmp/termination-log"

func main() {
	var cfg nodeprep.Config
	var printVersionAndExit bool

	flag.StringVar(&cfg.VolumeGroup, "volume-group", "containerstorage",
		"The LVM volume group to create or verify on the Azure resource disk. Must match the StorageClass volumeGroup parameter.")
	flag.StringVar(&cfg.ResourceDiskLink, "resource-disk-link", nodeprep.DefaultResourceDiskLink,
		"The stable udev link to the Azure ephemeral resource disk.")
	flag.BoolVar(&cfg.Required, "required", true,
		"Fail when the resource disk link is missing. When false, a missing link is logged and preparation is skipped.")
	flag.BoolVar(&cfg.AllowDestructivePreparation, "allow-destructive-preparation", false,
		"Acknowledge that the cloud-init filesystem on the resource disk, including all data under /mnt, will be erased.")
	flag.DurationVar(&cfg.CloudInitTimeout, "cloud-init-timeout", nodeprep.DefaultCloudInitTimeout,
		"Maximum time to wait for cloud-init to finish before failing.")
	flag.StringVar(&cfg.HostEtcDir, "host-etc-dir", nodeprep.DefaultHostEtcDir,
		"Mount point of the host /etc directory inside the container.")
	flag.StringVar(&cfg.LockFile, "lock-file", nodeprep.DefaultLockFile,
		"Host-wide lock file serializing node preparation. Must be on a mount shared with the host.")
	flag.BoolVar(&printVersionAndExit, "version", false, "Print version and exit")

	logConfig := textlogger.NewConfig(textlogger.VerbosityFlagName("v"))
	logConfig.AddFlags(flag.CommandLine)
	flag.Parse()

	logger := textlogger.NewLogger(logConfig)
	ctrl.SetLogger(logger)

	version.Log(logger)
	if printVersionAndExit {
		return
	}

	ctx := log.IntoContext(ctrl.SetupSignalHandler(), logger)

	preparer, err := nodeprep.New(cfg, nodeprep.NewHostRunner(utilexec.New()))
	if err != nil {
		logAndExit(err, "invalid node preparation configuration")
	}

	if err := preparer.Prepare(ctx); err != nil {
		logAndExit(err, "node preparation failed")
	}
	logger.Info("node preparation completed")
}

// logAndExit logs the error, writes it to the termination message file so
// it is surfaced by kubectl describe, and exits non-zero.
func logAndExit(err error, msg string) {
	ctrl.Log.Error(err, msg)
	errMsg := fmt.Sprintf("%s: %v", msg, err)
	if mkdirErr := os.MkdirAll(filepath.Dir(terminationMessagePath), 0755); mkdirErr != nil {
		ctrl.Log.Error(mkdirErr, "failed to create directory for termination message")
		os.Exit(1)
	}
	if writeErr := os.WriteFile(terminationMessagePath, []byte(errMsg), 0600); writeErr != nil {
		ctrl.Log.Error(writeErr, "failed to write termination message")
	}
	os.Exit(1)
}
