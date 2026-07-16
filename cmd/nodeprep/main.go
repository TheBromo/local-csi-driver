// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"local-csi-driver/internal/pkg/nodeprep"
	"local-csi-driver/internal/pkg/version"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		slog.Error("node preparation failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	config := nodeprep.DefaultConfig()
	lockPath := nodeprep.DefaultLockPath
	verbosity := 0
	printVersion := false

	flags := flag.NewFlagSet("local-csi-nodeprep", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&config.ResourceLink, "resource-disk", config.ResourceLink, "Stable host path for the Azure resource disk.")
	flags.BoolVar(&config.ResourceRequired, "resource-disk-required", config.ResourceRequired, "Fail when the resource disk link is absent.")
	flags.StringVar(&config.VolumeGroup, "volume-group", config.VolumeGroup, "LVM volume group to create or verify.")
	flags.StringVar(&config.VolumeGroupTag, "volume-group-tag", config.VolumeGroupTag, "Required ownership tag for the volume group.")
	flags.StringVar(&config.HostEtcPath, "host-etc-path", config.HostEtcPath, "Read/write mount of the host /etc directory.")
	flags.StringVar(&lockPath, "lock-path", lockPath, "Host-wide node-preparation lock path.")
	flags.DurationVar(&config.CloudInitTimeout, "cloud-init-timeout", config.CloudInitTimeout, "Maximum time to wait for cloud-init.")
	flags.IntVar(&verbosity, "v", verbosity, "Log verbosity level.")
	flags.BoolVar(&printVersion, "version", false, "Print version and exit.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}

	level := slog.LevelInfo
	if verbosity > 0 {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	info := version.GetInfo()
	logger.Info("version info",
		"buildId", info.BuildId,
		"version", info.Version,
		"gitCommit", info.GitCommit,
		"buildDate", info.BuildDate,
		"goVersion", info.GoVersion,
		"compiler", info.Compiler,
		"platform", info.Platform,
	)
	if printVersion {
		return nil
	}

	runner, err := nodeprep.NewHostRunner()
	if err != nil {
		return err
	}
	preparer, err := nodeprep.NewPreparer(
		config,
		runner,
		nodeprep.NewFileLocker(lockPath),
		nodeprep.NewFSTabEditor(config.HostEtcPath),
		logger,
	)
	if err != nil {
		return err
	}

	result, err := preparer.Prepare(ctx)
	if err != nil {
		return err
	}
	logger.Info("node preparation completed",
		"action", result.Action,
		"device", result.DevicePath,
		"majorMinor", result.MajorMinor,
		"volumeGroup", result.VolumeGroup,
	)
	return nil
}
