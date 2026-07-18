// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"flag"
	"fmt"
	"local-csi-driver/internal/pkg/nodeprep"
	"local-csi-driver/internal/pkg/version"
	"os"
	"path/filepath"

	"k8s.io/klog/v2/textlogger"
	utilexec "k8s.io/utils/exec"
	ctrl "sigs.k8s.io/controller-runtime"
)

// terminationMessagePath is the path to the termination message file for
// the Kubernetes pod. This file is used to store the last error message.
const terminationMessagePath = "/tmp/termination-log"

var (
	log = ctrl.Log
)

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

	ctrl.SetLogger(textlogger.NewLogger(logConfig))

	// Log version set by build process.
	version.Log(log)
	if printVersionAndExit {
		return
	}

	log.Info("node preparation started")

	preparer, err := nodeprep.New(cfg, nodeprep.NewHostRunner(utilexec.New()))
	if err != nil {
		logAndExit(err, "invalid node preparation configuration")
	}

	// Setup signal context
	ctx := ctrl.SetupSignalHandler()

	if err := preparer.Prepare(ctx); err != nil {
		logAndExit(err, "node preparation failed")
	}

	log.Info("node preparation completed")
}

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
