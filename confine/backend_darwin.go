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

// seatbeltSystem is what a program needs to read to start at all: the
// system's programs and libraries, its configuration and the devices.
var seatbeltSystem = []string{
	"/System",
	"/usr",
	"/bin",
	"/sbin",
	"/Library",
	"/dev",
	"/private/etc",
	"/private/var/db",
	"/private/var/select",
}

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

func (seatbelt) system() string {
	release, err := syscall.Sysctl("kern.osrelease")
	if err != nil {
		release = "unknown"
	}
	return "sandbox-exec on Darwin " + release
}

func (seatbelt) render(r resolved) ([]string, error) {
	profile, err := seatbeltProfile(r)
	if err != nil {
		return nil, err
	}
	return []string{seatbeltPath, "-p", profile}, nil
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

// seatbeltProfile renders the profile. A later rule overrides an earlier
// one, so everything is denied first and the allowed paths follow. The
// directories above an allowed path show their metadata and nothing else,
// which a program needs to find its working directory.
func seatbeltProfile(r resolved) (string, error) {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny network*)\n(deny file-write*)\n(deny file-read*)\n")
	b.WriteString("(allow file-write* (literal \"/dev/null\"))\n")

	read := append(append(append([]string{}, seatbeltSystem...), r.readOnly...), r.readWrite...)
	write := append([]string{}, r.readWrite...)
	if r.tmp != "" {
		read = append(read, r.tmp)
		write = append(write, r.tmp)
	}

	above := map[string]bool{}
	for _, path := range read {
		for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
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

	rules := []struct {
		rule  string
		paths []string
	}{
		{"(allow file-read-metadata (literal %s))\n", dirs},
		{"(allow file-read* (subpath %s))\n", read},
		{"(allow file-write* (subpath %s))\n", write},
	}
	for _, set := range rules {
		for _, path := range set.paths {
			quoted, err := seatbeltQuote(path)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, set.rule, quoted)
		}
	}
	return b.String(), nil
}
