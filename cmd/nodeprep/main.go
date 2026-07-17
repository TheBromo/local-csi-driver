// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"log"
	"os"
)

var errNotImplemented = errors.New("resource-disk preparation is not implemented")

func main() {
	log.Printf("node preparation failed: %v", errNotImplemented)
	os.Exit(1)
}
