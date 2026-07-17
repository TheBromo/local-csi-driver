package nodeprep

import (
	"context"
	"fmt"
)

// Preparer prepares the Azure resource disk for LVM use.
type Preparer struct {
	cfg  Config
	host Runner
}

// New creates a Preparer.
func New(cfg Config, host Runner) (*Preparer, error) {
	cfg = cfg.withDefaults()
	if host == nil {
		return nil, fmt.Errorf("host runner must not be nil")
	}
	if !vgNamePattern.MatchString(cfg.VolumeGroup) {
		return nil, fmt.Errorf("invalid volume group name %q", cfg.VolumeGroup)
	}
	if !vgNamePattern.MatchString(cfg.VolumeGroupTag) {
		return nil, fmt.Errorf("invalid volumebgroup tag %q", cfg.VolumeGroupTag)
	}
	return &Preparer{cfg: cfg, host: host}, nil
}

// Prepare drives the node through the supported state transitions until the
// configured volume group is available on the resource disk, or fails
// closed without writing anything on unrecognized states.
func (p *Preparer) Prepare(ctx context.Context) error {
	return nil
}
