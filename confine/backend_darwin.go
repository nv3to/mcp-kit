//go:build darwin

package confine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// seatbeltPath is the program that applies a Seatbelt profile and then
// becomes the command, /usr/bin/sandbox-exec.
var seatbeltPath = "/usr/bin/sandbox-exec"

// seatbeltSystem is what every program needs to run at all: the system, its
// libraries and its own programs. A command may read and execute there.
var seatbeltSystem = []string{
	"/System",
	"/bin",
	"/private/var/db/dyld",
	"/private/var/db/timezone",
	"/private/var/select",
	"/sbin",
	"/usr/bin",
	"/usr/lib",
	"/usr/libexec",
	"/usr/sbin",
	"/usr/share",
}

// seatbeltFiles are single files a program reads as it starts. The dynamic
// linker reads the root directory, and a program that may not is killed
// before it runs.
var seatbeltFiles = []string{
	"/",
	"/dev/null",
	"/dev/random",
	"/dev/urandom",
	"/dev/zero",
	"/private/etc/localtime",
}

// seatbeltLinks are the links at the root and the directories they lead to.
// They show their metadata, so a path through them can be followed.
var seatbeltLinks = []string{
	"/etc",
	"/private",
	"/private/etc",
	"/private/tmp",
	"/private/var",
	"/tmp",
	"/var",
}

// seatbeltCertificates is where the certificates of the system lie. A
// command that may connect may read them, to verify the host behind a
// tunnel. The daemon that evaluates trust stays out of reach: it fetches
// what a certificate names, from outside the sandbox.
const seatbeltCertificates = "/private/etc/ssl"

// seatbeltSysctl is what a program may ask the kernel: the machine and the
// system, and nothing of other processes.
const seatbeltSysctl = "(allow sysctl-read (sysctl-name-prefix \"hw.\") (sysctl-name-prefix \"machdep.\")" +
	" (sysctl-name-prefix \"vm.\") (sysctl-name-prefix \"kern.os\") (sysctl-name \"kern.version\")" +
	" (sysctl-name \"kern.argmax\") (sysctl-name \"kern.hostname\") (sysctl-name \"kern.maxfilesperproc\")" +
	" (sysctl-name \"kern.ngroups\") (sysctl-name \"kern.usrstack64\") (sysctl-name \"kern.secure_kernel\")" +
	" (sysctl-name \"sysctl.proc_cputype\") (sysctl-name \"sysctl.proc_native\"))\n"

// seatbeltNested is a profile that restricts something. A sandbox that
// restricts something refuses it with status 71 (docs/confinement-macos.md,
// probe 3).
const seatbeltNested = "(version 1)\n(allow default)\n(deny file-read* (literal \"/private/var/empty/mcpkit-confine\"))\n"

// seatbeltRefused is the status of sandbox-exec when the profile could not
// be applied.
const seatbeltRefused = 71

type seatbelt struct{}

var seatbeltConfined = sync.OnceValue(func() bool {
	err := exec.Command(seatbeltPath, "-p", seatbeltNested, "/usr/bin/true").Run()
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == seatbeltRefused
})

func pick() (backend, error) { return seatbelt{}, nil }

func (seatbelt) available() error {
	info, err := os.Stat(seatbeltPath)
	if err != nil {
		return fmt.Errorf("the sandbox is missing: %v", err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("the sandbox is missing: %s is not a program", seatbeltPath)
	}
	return nil
}

func (seatbelt) confined() bool { return seatbeltConfined() }

func (seatbelt) relays() bool { return false }

func (seatbelt) system() string {
	release, err := syscall.Sysctl("kern.osrelease")
	if err != nil {
		release = "unknown"
	}
	return "sandbox-exec, deny by default, on Darwin " + release
}

func (seatbelt) render(r resolved, argv []string) ([]string, error) {
	profile, err := seatbeltProfile(r)
	if err != nil {
		return nil, err
	}
	return append([]string{seatbeltPath, "-p", profile}, argv...), nil
}

// seatbeltQuote writes path as a string of the profile language. A control
// character has no escape that is known to be read back as itself, so a path
// that holds one cannot be expressed.
func seatbeltQuote(path string) (string, error) {
	for i := 0; i < len(path); i++ {
		if path[i] < 0x20 || path[i] == 0x7f {
			return "", fmt.Errorf("the path %q holds a control character, which a Seatbelt profile cannot express", path)
		}
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`, nil
}

// seatbeltRule is one path of the profile and whether it may be written.
type seatbeltRule struct {
	path  string
	write bool
}

// seatbeltProfile renders the profile. Everything is denied that no rule
// allows. A later rule overrides an earlier one, so the paths are written
// from the shortest to the longest: a path inside another follows it, and a
// read-only path inside a writable one stays read-only, as do the
// directories that lead to it. The directories above
// a path show their metadata and nothing else, which a program needs to find
// its working directory. Seatbelt names a loopback address as localhost and
// in no other way, so the proxy is a port there (docs/confinement-macos.md,
// probe 1).
func seatbeltProfile(r resolved) (string, error) {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process-fork)\n(allow signal (target same-sandbox))\n")
	b.WriteString(seatbeltSysctl)
	b.WriteString("(allow file-write* (literal \"/dev/null\"))\n")
	if r.proxy.IsValid() {
		if !r.proxy.Addr().IsLoopback() {
			return "", fmt.Errorf("the proxy %s is not on a loopback address, which a Seatbelt profile cannot express", r.proxy)
		}
		fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", r.proxy.Port())
		fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", seatbeltCertificates)
	}

	var paths []seatbeltRule
	for _, path := range r.readOnly {
		paths = append(paths, seatbeltRule{path, false})
	}
	for _, path := range r.readWrite {
		paths = append(paths, seatbeltRule{path, true})
	}
	if r.tmp != "" {
		paths = append(paths, seatbeltRule{r.tmp, true})
	}
	for _, rule := range paths {
		for _, system := range seatbeltSystem {
			if rule.write && within(system, rule.path) {
				return "", fmt.Errorf("the writable path %s lies inside the system, in %s", rule.path, system)
			}
		}
	}
	// Of two rules for one path the read-only one is the later.
	sort.SliceStable(paths, func(i, j int) bool {
		if len(paths[i].path) != len(paths[j].path) {
			return len(paths[i].path) < len(paths[j].path)
		}
		return paths[i].write && !paths[j].write
	})

	// A rule holds for the path a file has now. A directory above a
	// read-only path must stay where it is, or the path below it gets
	// another name, which no rule protects.
	fixed := map[string]bool{}
	for _, rule := range paths {
		if rule.write {
			continue
		}
		for dir := filepath.Dir(rule.path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			for _, other := range paths {
				if other.write && within(other.path, dir) {
					fixed[dir] = true
				}
			}
		}
	}
	var held []string
	for dir := range fixed {
		held = append(held, dir)
	}
	sort.Strings(held)

	above := map[string]bool{}
	for _, rule := range paths {
		for dir := filepath.Dir(rule.path); ; dir = filepath.Dir(dir) {
			above[dir] = true
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	var dirs []string
	for dir := range above {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	base := []struct {
		rule  string
		paths []string
	}{
		{"(allow file-read* process-exec (subpath %s))\n", seatbeltSystem},
		{"(allow file-read* (literal %s))\n", seatbeltFiles},
		{"(allow file-read-metadata (literal %s))\n", seatbeltLinks},
		{"(allow file-read-metadata (literal %s))\n", dirs},
	}
	for _, set := range base {
		for _, path := range set.paths {
			quoted, err := seatbeltQuote(path)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, set.rule, quoted)
		}
	}
	for _, rule := range paths {
		quoted, err := seatbeltQuote(rule.path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "(allow file-read* process-exec (subpath %s))\n", quoted)
		if rule.write {
			fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", quoted)
		} else {
			fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", quoted)
		}
	}
	for _, dir := range held {
		quoted, err := seatbeltQuote(dir)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "(deny file-write* (literal %s))\n", quoted)
	}
	return b.String(), nil
}
