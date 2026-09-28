//go:build !darwin

package confine

import (
	"fmt"
	"runtime"
)

// pick finds no sandbox on this system, so Command refuses every command.
func pick() (backend, error) {
	return nil, fmt.Errorf("there is no sandbox for %s", runtime.GOOS)
}
