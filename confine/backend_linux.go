//go:build linux

package confine

// pick chooses bubblewrap, the sandbox of Linux.
func pick() (backend, error) { return bubblewrap{}, nil }
