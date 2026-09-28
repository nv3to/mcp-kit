package confine

// The bubblewrap backend builds on every system, so that its rendering is
// tested where the package is developed; pick chooses it on Linux alone.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// bwrapName is the program of bubblewrap, looked up on the PATH.
var bwrapName = "bwrap"

// bwrapEnv starts the command inside the sandbox. bubblewrap sets PWD, which
// no profile names, and env removes it again.
const bwrapEnv = "/usr/bin/env"

// bwrapSysctl is where the kernel shows its settings.
var bwrapSysctl = "/proc/sys"

// bwrapTrial bounds the sandbox that available starts to see that one can
// start at all.
const bwrapTrial = 10 * time.Second

// bwrapSystem is what every program needs to run at all: the programs and
// libraries of the system. A command may read and execute there. A path that
// is a symlink on the host, as /bin is where /usr is merged, is the same
// symlink inside.
var bwrapSystem = []string{
	"/bin",
	"/lib",
	"/lib32",
	"/lib64",
	"/libx32",
	"/sbin",
	"/usr/bin",
	"/usr/lib",
	"/usr/lib32",
	"/usr/lib64",
	"/usr/libexec",
	"/usr/libx32",
	"/usr/sbin",
	"/usr/share",
}

// bwrapFiles are what a program reads of /etc as it starts: the cache of the
// dynamic loader, the time zone, and the links through which Debian names
// some of its programs. Nothing else of /etc is there.
var bwrapFiles = []string{
	"/etc/alternatives",
	"/etc/ld.so.cache",
	"/etc/localtime",
}

// bwrapUserns are the settings of the kernel that refuse the user namespace
// bubblewrap needs, and the value that refuses it.
var bwrapUserns = []struct{ name, refuses string }{
	{"kernel.apparmor_restrict_unprivileged_userns", "1"},
	{"kernel.unprivileged_userns_clone", "0"},
	{"user.max_user_namespaces", "0"},
}

// bwrapHost is the map of user ids outside every user namespace: each id is
// itself.
const bwrapHost = "0 0 4294967295"

type bubblewrap struct{}

// available starts a sandbox under the empty profile, because bubblewrap
// that is installed may still be refused the namespaces it needs.
func (b bubblewrap) available() error {
	if _, err := os.Stat(bwrapEnv); err != nil {
		return fmt.Errorf("the sandbox is missing: %v", err)
	}
	argv, err := b.render(resolved{}, []string{"/usr/bin/true"})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), bwrapTrial)
	defer cancel()
	trial := exec.CommandContext(ctx, argv[0], argv[1:]...)
	trial.Env = []string{}
	if out, err := trial.CombinedOutput(); err != nil {
		return bwrapRefused(err, string(out))
	}
	return nil
}

// bwrapRefused says why bubblewrap could not start a sandbox. What it prints
// differs with the cause, so the settings that refuse a user namespace are
// named from their values.
func bwrapRefused(failed error, output string) error {
	reason := strings.TrimSpace(output)
	if reason == "" {
		reason = failed.Error()
	}
	var refusing []string
	for _, setting := range bwrapUserns {
		data, err := os.ReadFile(filepath.Join(bwrapSysctl, strings.ReplaceAll(setting.name, ".", "/")))
		if err == nil && strings.TrimSpace(string(data)) == setting.refuses {
			refusing = append(refusing, setting.name+" = "+setting.refuses)
		}
	}
	if len(refusing) > 0 {
		return fmt.Errorf("bubblewrap cannot start a sandbox: %s; the system refuses unprivileged user namespaces, which bubblewrap needs (%s), and docs/confinement-linux.md says how a human allows them",
			reason, strings.Join(refusing, ", "))
	}
	return fmt.Errorf("bubblewrap cannot start a sandbox: %s; docs/confinement-linux.md says what it needs", reason)
}

// confined reads the map of user ids. bubblewrap, like a container, maps a
// few ids and not every id to itself. A map that cannot be read counts as
// confined, so that Command refuses.
func (bubblewrap) confined() bool {
	data, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return true
	}
	return strings.Join(strings.Fields(string(data)), " ") != bwrapHost
}

func (bubblewrap) render(r resolved, argv []string) ([]string, error) {
	program, err := exec.LookPath(bwrapName)
	if err != nil {
		return nil, fmt.Errorf("the sandbox is missing: %v", err)
	}
	return bwrapArgs(program, bwrapLayout(), r, argv)
}

func (bubblewrap) system() string {
	version := "bubblewrap of an unknown version"
	if program, err := exec.LookPath(bwrapName); err == nil {
		if out, err := exec.Command(program, "--version").Output(); err == nil {
			version = strings.TrimSpace(string(out))
		}
	}
	release := "unknown"
	if data, err := os.ReadFile(filepath.Join(bwrapSysctl, "kernel", "osrelease")); err == nil {
		release = strings.TrimSpace(string(data))
	}
	return version + ", deny by default, on Linux " + release
}

// bwrapEntry is a path of the system as the host has it: a directory or a
// file, or a symlink and its target.
type bwrapEntry struct {
	path string
	link string
}

// bwrapLayout finds the paths of bwrapSystem and bwrapFiles on this host.
func bwrapLayout() []bwrapEntry {
	var layout []bwrapEntry
	for _, path := range append(append([]string{}, bwrapSystem...), bwrapFiles...) {
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		entry := bwrapEntry{path: path}
		if info.Mode()&os.ModeSymlink != 0 {
			if entry.link, err = os.Readlink(path); err != nil {
				continue
			}
		}
		layout = append(layout, entry)
	}
	return layout
}

// bwrapRule is one path the sandbox binds from the host and whether it may
// be written.
type bwrapRule struct {
	path  string
	write bool
}

// bwrapDecides returns the rule that decides for path: the longest that
// holds it, and of two for the same path the read-only one.
func bwrapDecides(rules []bwrapRule, path string) (decides bwrapRule, ok bool) {
	for _, rule := range rules {
		if !within(rule.path, path) {
			continue
		}
		if !ok || len(rule.path) > len(decides.path) || (rule.path == decides.path && !rule.write) {
			decides, ok = rule, true
		}
	}
	return decides, ok
}

// bwrapMount is one step that builds the filesystem of the sandbox. Of two
// steps at one path the one of the higher rank is the later and wins.
type bwrapMount struct {
	path string
	rank int
	args []string
}

// bwrapArgs renders the profile. The root of the sandbox is empty, and the
// system of layout and the paths of the profile are bound into it at the
// paths they have on the host. The binds are made from the shortest path to
// the longest, so a path inside another is bound over it: a read-only path
// inside a writable one stays read-only, and a writable path inside a
// read-only one can be written.
//
// A mount point cannot be moved, so a writable path cannot be either. A
// writable directory above a read-only path is bound onto itself, so that it
// cannot be moved and give the path below it another name.
//
// The network is always a namespace of its own, which holds its own loopback
// alone (docs/confinement-linux.md, probe 1): a proxy on the loopback of the
// host is out of reach there.
func bwrapArgs(program string, layout []bwrapEntry, r resolved, argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("there is no program to run")
	}
	if strings.Contains(argv[0], "=") {
		return nil, fmt.Errorf("the program %q holds an equals sign, which %s would take for a variable", argv[0], bwrapEnv)
	}
	var rules []bwrapRule
	for _, path := range r.readOnly {
		rules = append(rules, bwrapRule{path, false})
	}
	for _, path := range r.readWrite {
		rules = append(rules, bwrapRule{path, true})
	}
	if r.tmp != "" {
		rules = append(rules, bwrapRule{r.tmp, true})
	}
	for _, rule := range rules {
		for _, system := range append(append([]string{}, bwrapSystem...), bwrapFiles...) {
			if rule.write && within(system, rule.path) {
				return nil, fmt.Errorf("the writable path %s lies inside the system, in %s", rule.path, system)
			}
		}
	}
	mounts := []bwrapMount{
		{"/dev", 2, []string{"--dev", "/dev"}},
		{"/proc", 2, []string{"--proc", "/proc"}},
	}
	for _, entry := range layout {
		if entry.link != "" {
			mounts = append(mounts, bwrapMount{entry.path, 2, []string{"--symlink", entry.link, entry.path}})
		} else {
			rules = append(rules, bwrapRule{entry.path, false})
		}
	}

	bound := map[string]bool{}
	for _, rule := range rules {
		bound[rule.path] = true
	}
	var held []bwrapRule
	for _, rule := range rules {
		if rule.write {
			continue
		}
		for dir := filepath.Dir(rule.path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if bound[dir] {
				continue
			}
			if decides, ok := bwrapDecides(rules, dir); ok && decides.write {
				bound[dir] = true
				held = append(held, bwrapRule{dir, true})
			}
		}
	}

	for _, rule := range append(rules, held...) {
		if rule.write {
			mounts = append(mounts, bwrapMount{rule.path, 0, []string{"--bind", rule.path, rule.path}})
		} else {
			mounts = append(mounts, bwrapMount{rule.path, 1, []string{"--ro-bind", rule.path, rule.path}})
		}
	}
	sort.Slice(mounts, func(i, j int) bool {
		a, b := mounts[i], mounts[j]
		switch {
		case len(a.path) != len(b.path):
			return len(a.path) < len(b.path)
		case a.path != b.path:
			return a.path < b.path
		case a.rank != b.rank:
			return a.rank < b.rank
		}
		return strings.Join(a.args, "\x00") < strings.Join(b.args, "\x00")
	})

	args := []string{
		program,
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup",
		"--unshare-net",
		"--new-session",
		"--die-with-parent",
	}
	last := ""
	for _, mount := range mounts {
		step := strings.Join(mount.args, "\x00")
		if step == last {
			continue
		}
		last = step
		args = append(args, mount.args...)
	}
	if r.dir != "" {
		args = append(args, "--chdir", r.dir)
	}
	args = append(args, "--", bwrapEnv, "-u", "PWD")
	return append(args, argv...), nil
}
