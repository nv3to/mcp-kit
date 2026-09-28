// Package confine starts a command that can touch only what a Profile names:
// the files it lists, the environment it allows and a temporary directory of
// the command's own. The network is closed, or narrowed to one proxy. A
// command that cannot be held to its profile is refused, never started
// unconfined.
//
// The sandbox denies what no rule allows. Beside the profile, a command can
// read and execute the system itself, which every program needs to run: on
// macOS /System, /bin, /sbin, /usr/bin, /usr/sbin, /usr/lib, /usr/libexec,
// /usr/share, /private/var/db/dyld and /private/var/select. It cannot read
// /etc, /Library, /Applications or a home directory, it cannot list the
// processes of the host, and it cannot reach a service of the system. With a
// proxy it can read the certificates in /private/etc/ssl.
//
// The package confines the commands a server starts. It does not confine the
// server itself.
package confine

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	mcpkit "github.com/nv3to/mcp-kit"
)

// Marker is the variable every confined command finds set in its
// environment. Confined reads it, so a server started by a confined command
// knows it cannot start a sandbox of its own.
const Marker = "MCPKIT_CONFINED"

// TempVar is the variable that names the command's private temporary
// directory.
const TempVar = "TMPDIR"

// killGrace bounds how long Wait lingers for output once the command is gone,
// so a process the command left behind cannot hold Wait through a pipe.
const killGrace = 5 * time.Second

// proxyVars are the variables that point a program at a proxy, in both
// spellings. noProxyVars exempt hosts from it and are set empty.
var (
	proxyVars   = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"}
	noProxyVars = []string{"NO_PROXY", "no_proxy"}
)

// Profile states what a confined command may touch. Anything it does not
// name is out of reach, except the system itself as the package comment lists
// it. The zero Profile allows the command's own temporary directory and
// nothing else. Where one path lies inside another, the longer one decides:
// a ReadOnly path inside a ReadWrite path cannot be written, and the
// directories that lead to it cannot be moved. A path in both lists is
// read-only.
type Profile struct {
	// ReadOnly lists the files and directories the command may read.
	ReadOnly []string
	// ReadWrite lists the files and directories the command may read and
	// write.
	ReadWrite []string
	// Env is the command's environment.
	Env Env
	// Network says what the command may reach. The zero value is None.
	Network Network
}

// Env describes the environment of a confined command. Every variable it
// does not name is absent, so a secret in the server's environment stays
// there.
type Env struct {
	// Pass names the variables copied from the server's environment. A
	// name the server does not have set is left out.
	Pass []string
	// Set holds fixed names and values. It wins over Pass.
	Set map[string]string
}

// Network says what a confined command may reach. It is None or comes from
// Proxy.
type Network struct {
	// narrowed tells a proxy with an empty address from None.
	narrowed bool
	proxy    string
}

// None closes the network: the command can open no connection at all.
var None = Network{}

// Proxy narrows the network to one destination: addr, the address an egress
// proxy listens on, as a loopback address and a port such as
// "127.0.0.1:3128". The command can connect there and nowhere else, cannot
// listen and cannot resolve a name, so what the proxy allows is all the
// command reaches. The command finds the proxy in HTTP_PROXY, HTTPS_PROXY and
// their lower-case spellings, beside an empty NO_PROXY; Env may not name
// these. An address that is not a loopback address with a port makes Command
// fail with kind invalid.
func Proxy(addr string) Network {
	return Network{narrowed: true, proxy: addr}
}

// Cmd is a confined command that has not run yet. It is used like an
// exec.Cmd and runs once.
type Cmd struct {
	// Stdout receives the command's standard output; nil discards it.
	Stdout io.Writer
	// Stderr receives the command's standard error; nil discards it.
	Stderr io.Writer
	// Dir is the working directory. It must lie inside a path the profile
	// makes readable; empty means the private temporary directory.
	Dir string

	ctx   context.Context
	via   backend
	paths resolved
	env   []string
	path  string
	args  []string
	tmp   string
	cmd   *exec.Cmd
}

// resolved is a profile as a backend renders it: every path absolute, free
// of symlinks and known to exist.
type resolved struct {
	readOnly  []string
	readWrite []string
	tmp       string
	// proxy is the one address the command may connect to; the zero value
	// closes the network.
	proxy netip.AddrPort
}

// readable reports whether path lies inside a path the profile can read.
func (r resolved) readable(path string) bool {
	for _, list := range [][]string{r.readOnly, r.readWrite, {r.tmp}} {
		for _, parent := range list {
			if parent != "" && within(parent, path) {
				return true
			}
		}
	}
	return false
}

// within reports whether child is parent or lies below it.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolvePath makes path absolute and follows its symlinks, because a
// backend matches the path a file really has. A rule for a path that does
// not exist would be dropped in silence, so it is an error instead.
func resolvePath(path string) (string, error) {
	if path == "" {
		return "", mcpkit.Errorf(mcpkit.Invalid, "confine: an empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", mcpkit.Errorf(mcpkit.Invalid, "confine: path %q: %v", path, err)
	}
	target, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", mcpkit.Errorf(mcpkit.NotFound, "confine: path %q does not exist", path)
	}
	if err != nil {
		return "", mcpkit.Errorf(mcpkit.Invalid, "confine: path %q: %v", path, err)
	}
	return target, nil
}

func resolveAll(paths []string) ([]string, error) {
	var out []string
	for _, path := range paths {
		target, err := resolvePath(path)
		if err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	return out, nil
}

func (p Profile) resolve() (resolved, error) {
	readOnly, err := resolveAll(p.ReadOnly)
	if err != nil {
		return resolved{}, err
	}
	readWrite, err := resolveAll(p.ReadWrite)
	if err != nil {
		return resolved{}, err
	}
	proxy, err := p.Network.resolve()
	if err != nil {
		return resolved{}, err
	}
	return resolved{readOnly: readOnly, readWrite: readWrite, proxy: proxy}, nil
}

// resolve parses the address of the proxy. A name is not accepted, because
// the command and the sandbox must mean the same address by it.
func (n Network) resolve() (netip.AddrPort, error) {
	if !n.narrowed {
		return netip.AddrPort{}, nil
	}
	addr, err := netip.ParseAddrPort(n.proxy)
	if err != nil {
		return netip.AddrPort{}, mcpkit.Errorf(mcpkit.Invalid, "confine: the proxy %q is not an address and a port", n.proxy)
	}
	if !addr.Addr().IsLoopback() || addr.Port() == 0 {
		return netip.AddrPort{}, mcpkit.Errorf(mcpkit.Invalid, "confine: the proxy %s is not a port of a loopback address", n.proxy)
	}
	return addr, nil
}

// proxyEnv is what points the command at its proxy: nothing when the
// network is closed.
func (r resolved) proxyEnv() []string {
	if !r.proxy.IsValid() {
		return nil
	}
	var env []string
	for _, name := range proxyVars {
		env = append(env, name+"=http://"+r.proxy.String())
	}
	for _, name := range noProxyVars {
		env = append(env, name+"=")
	}
	return env
}

func checkName(name string) error {
	switch {
	case name == "" || strings.ContainsAny(name, "=\x00"):
		return mcpkit.Errorf(mcpkit.Invalid, "confine: %q is not the name of a variable", name)
	case name == Marker || name == TempVar:
		return mcpkit.Errorf(mcpkit.Invalid, "confine: the variable %s is set by confine", name)
	}
	for _, list := range [][]string{proxyVars, noProxyVars} {
		for _, set := range list {
			if name == set {
				return mcpkit.Errorf(mcpkit.Invalid, "confine: the variable %s is set by confine from Profile.Network", name)
			}
		}
	}
	return nil
}

// build returns the environment as exec wants it, sorted by name.
func (e Env) build() ([]string, error) {
	vars := map[string]string{}
	for _, name := range e.Pass {
		if err := checkName(name); err != nil {
			return nil, err
		}
		if value, ok := os.LookupEnv(name); ok {
			vars[name] = value
		}
	}
	for name, value := range e.Set {
		if err := checkName(name); err != nil {
			return nil, err
		}
		if strings.Contains(value, "\x00") {
			return nil, mcpkit.Errorf(mcpkit.Invalid, "confine: the value of %s holds a zero byte", name)
		}
		vars[name] = value
	}
	var env []string
	for name, value := range vars {
		env = append(env, name+"="+value)
	}
	sort.Strings(env)
	return env, nil
}

// Confined reports whether this process already runs under a sandbox: one
// that confine started, which sets Marker, or any other that refuses a
// sandbox inside it. Command refuses in a confined process.
func Confined() bool {
	if os.Getenv(Marker) != "" {
		return true
	}
	b, err := pick()
	return err == nil && b.confined()
}

// Command returns the command name with args, confined to profile. Nothing
// runs until Start or Run. The name is looked up on the server's PATH; the
// file it names must lie where the command may read.
//
// Command fails closed. The error is of kind refused, and no process was
// started, when the sandbox of this system is missing, when it cannot
// express the profile, when this process is itself confined, or when the
// profile makes a directory given to Verify or Verified writable. A path
// that does not exist is not_found. A proxy that is not a port of a loopback
// address is invalid, and so is a variable in Env that confine sets itself.
func Command(ctx context.Context, profile Profile, name string, args ...string) (*Cmd, error) {
	b, err := pick()
	if err != nil {
		return nil, mcpkit.Errorf(mcpkit.Refused, "confine: %v", err)
	}
	if err := b.available(); err != nil {
		return nil, mcpkit.Errorf(mcpkit.Refused, "confine: %v", err)
	}
	if Confined() {
		return nil, mcpkit.Errorf(mcpkit.Refused, "confine: this process is confined, and a sandbox cannot start inside a sandbox")
	}
	paths, err := profile.resolve()
	if err != nil {
		return nil, err
	}
	for _, dir := range verdictDirs() {
		for _, writable := range paths.readWrite {
			if within(writable, dir) || within(dir, writable) {
				return nil, mcpkit.Errorf(mcpkit.Refused, "confine: the writable path %s reaches the verdict in %s", writable, dir)
			}
		}
	}
	env, err := profile.Env.build()
	if err != nil {
		return nil, err
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, mcpkit.Errorf(mcpkit.NotFound, "confine: command %q: %v", name, err)
	}
	if path, err = filepath.Abs(path); err != nil {
		return nil, mcpkit.Errorf(mcpkit.Invalid, "confine: command %q: %v", name, err)
	}
	// The temporary directory does not exist yet. Its parent stands in for
	// it, so a profile the backend cannot express is refused here.
	trial := paths
	if trial.tmp, err = resolvePath(os.TempDir()); err != nil {
		return nil, err
	}
	if _, err := b.render(trial); err != nil {
		return nil, mcpkit.Errorf(mcpkit.Refused, "confine: %v", err)
	}
	return &Cmd{ctx: ctx, via: b, paths: paths, env: env, path: path, args: args}, nil
}

// Start creates the private temporary directory and starts the command. A
// command that was started must be waited for: Wait removes the directory.
func (c *Cmd) Start() error {
	if c.via == nil {
		return mcpkit.Errorf(mcpkit.Invalid, "confine: a Cmd comes from Command")
	}
	if c.cmd != nil {
		return mcpkit.Errorf(mcpkit.Conflict, "confine: the command was already started")
	}
	tmp, err := os.MkdirTemp("", "confine-")
	if err != nil {
		return mcpkit.Errorf(mcpkit.Internal, "confine: temporary directory: %v", err)
	}
	if err := c.start(tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return nil
}

func (c *Cmd) start(tmp string) error {
	paths := c.paths
	var err error
	if paths.tmp, err = resolvePath(tmp); err != nil {
		return err
	}
	dir := paths.tmp
	if c.Dir != "" {
		if dir, err = resolvePath(c.Dir); err != nil {
			return err
		}
		if !paths.readable(dir) {
			return mcpkit.Errorf(mcpkit.Invalid, "confine: the working directory %s is not inside a readable path", c.Dir)
		}
	}
	argv, err := c.via.render(paths)
	if err != nil {
		return mcpkit.Errorf(mcpkit.Refused, "confine: %v", err)
	}
	argv = append(append(argv, c.path), c.args...)
	cmd := exec.CommandContext(c.ctx, argv[0], argv[1:]...)
	cmd.Env = append(append([]string{}, c.env...), paths.proxyEnv()...)
	cmd.Env = append(cmd.Env, TempVar+"="+paths.tmp, Marker+"=1")
	cmd.Dir = dir
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	cmd.WaitDelay = killGrace
	if err := cmd.Start(); err != nil {
		return mcpkit.Errorf(mcpkit.Internal, "confine: start: %v", err)
	}
	c.cmd = cmd
	c.tmp = paths.tmp
	return nil
}

// Wait waits for the command to end and removes its temporary directory,
// also when the command was killed through the context. The error is the
// one exec.Cmd.Wait returns.
func (c *Cmd) Wait() error {
	if c.cmd == nil {
		return mcpkit.Errorf(mcpkit.Conflict, "confine: the command was not started")
	}
	err := c.cmd.Wait()
	if removeErr := os.RemoveAll(c.tmp); err == nil && removeErr != nil {
		err = mcpkit.Errorf(mcpkit.Internal, "confine: temporary directory: %v", removeErr)
	}
	return err
}

// Run starts the command and waits for it.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}
