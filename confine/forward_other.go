//go:build !linux

package confine

import (
	"errors"
	"fmt"
	"os"
)

// forwardable refuses: only bubblewrap, on Linux, needs a forwarder.
func forwardable() error {
	return errors.New("the forwarder runs on Linux alone")
}

// forward refuses, as forwardable does.
func forward([]string) int {
	fmt.Fprintf(os.Stderr, "confine: %v\n", forwardable())
	return forwardFailed
}
