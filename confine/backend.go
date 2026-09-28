package confine

// backend is the sandbox of one operating system. pick chooses it; a system
// without one has no backend, and Command refuses there.
type backend interface {
	// available says why the sandbox cannot be started, or nil.
	available() error
	// confined reports whether this process is inside a sandbox already.
	confined() bool
	// render returns the command line that runs argv, the program and its
	// arguments, inside the sandbox of the profile. It starts nothing.
	render(r resolved, argv []string) ([]string, error)
	// relays reports whether a command reaches its proxy through the unix
	// socket of a relay, because its network is a namespace of its own.
	relays() bool
	// system names the sandbox and the system a verdict holds for.
	system() string
}
