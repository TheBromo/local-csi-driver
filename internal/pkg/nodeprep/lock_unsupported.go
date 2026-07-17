// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package nodeprep

import (
	"context"
	"fmt"
)

// acquireLock is only supported on Linux hosts.
func acquireLock(_ context.Context, path string) (func(), error) {
	return nil, fmt.Errorf("host locking is not supported on this platform (lock file %s)", path)
}
