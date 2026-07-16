// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunVersionDoesNotTouchHost(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &output); err != nil {
		t.Fatalf("run(--version) error = %v", err)
	}
	if !strings.Contains(output.String(), "version info") {
		t.Fatalf("run(--version) output = %q, want version info", output.String())
	}
}

func TestRunRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	err := run(context.Background(), []string{"unexpected"}, &output)
	if err == nil || !strings.Contains(err.Error(), "unexpected positional arguments") {
		t.Fatalf("run(positional) error = %v, want positional argument rejection", err)
	}
}
