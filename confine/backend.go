package confine

// backend is the sandbox of one operating system. pick chooses it; a system
// without one has no backend, and Command refuses there.
type backend interface {
	// available says why the sandbox cannot be started, or nil.
	available() error
	// confined reports whether this process is inside a sandbox already.
	confined() bool
	// render returns the arguments that go before the command. It is a
	// pure function of the profile and starts nothing.
	render(r resolved) ([]string, error)
	// system names the sandbox and the system a verdict holds for.
	system() string
}
