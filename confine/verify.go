package confine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mcpkit "github.com/nv3to/mcp-kit"
)

// verdictFile is the name of the verdict inside the directory given to
// Verify.
const verdictFile = "confine-verdict.json"

// verifyCanary is what the probes plant outside the profile and must never
// read back.
const verifyCanary = "mcpkit-confine-verify-canary"

// verdict is what Verify records.
type verdict struct {
	// OK is whether every probe held.
	OK bool `json:"ok"`
	// Reason names the probe that did not hold.
	Reason string `json:"reason"`
	// System is the sandbox and the system the probes ran on.
	System string `json:"system"`
	// At is when they ran.
	At time.Time `json:"at"`
}

var verdicts struct {
	sync.Mutex
	dirs []string
}

// keep records a verdict directory, so that Command can refuse a profile
// that makes it writable.
func keep(dir string) {
	verdicts.Lock()
	defer verdicts.Unlock()
	for _, known := range verdicts.dirs {
		if known == dir {
			return
		}
	}
	verdicts.dirs = append(verdicts.dirs, dir)
}

func verdictDirs() []string {
	verdicts.Lock()
	defer verdicts.Unlock()
	return append([]string{}, verdicts.dirs...)
}

// Verify checks that the sandbox of this system holds: it runs commands
// under a profile and watches them fail to read and to write outside it. The
// verdict is recorded in dir, where Verified finds it after a restart.
//
// Keep dir out of the writable paths of every profile, or a confined command
// could write the verdict itself. Command refuses such a profile once dir was
// given to Verify or Verified.
//
// A sandbox that does not hold is recorded and returned as an error of kind
// refused. When the probes cannot run at all, nothing is recorded.
func Verify(ctx context.Context, dir string) error {
	target, err := resolvePath(dir)
	if err != nil {
		return err
	}
	keep(target)
	b, err := pick()
	if err != nil {
		return mcpkit.Errorf(mcpkit.Refused, "confine: %v", err)
	}
	reason, err := probe(ctx)
	if err != nil {
		return err
	}
	v := verdict{OK: reason == "", Reason: reason, System: b.system(), At: time.Now().UTC()}
	if v.OK {
		v.Reason = "every probe held"
	}
	data, err := json.Marshal(v)
	if err != nil {
		return mcpkit.Errorf(mcpkit.Internal, "confine: verdict: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, verdictFile), data, 0o600); err != nil {
		return mcpkit.Errorf(mcpkit.Internal, "confine: verdict: %v", err)
	}
	if !v.OK {
		return mcpkit.Errorf(mcpkit.Refused, "confine: the sandbox does not hold: %s", reason)
	}
	return nil
}

// Verified reads the verdict that Verify recorded in dir. It is false, with
// the reason, when there is none, when the probes did not hold, or when the
// verdict is for another system than this one, as after an upgrade.
func Verified(dir string) (ok bool, reason string) {
	target, err := resolvePath(dir)
	if err != nil {
		return false, err.Error()
	}
	keep(target)
	data, err := os.ReadFile(filepath.Join(target, verdictFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, "no verdict is recorded in " + dir
	}
	if err != nil {
		return false, err.Error()
	}
	var v verdict
	if err := json.Unmarshal(data, &v); err != nil {
		return false, "the verdict in " + dir + " cannot be read: " + err.Error()
	}
	b, err := pick()
	if err != nil {
		return false, err.Error()
	}
	if now := b.system(); v.System != now {
		return false, "the verdict is for " + v.System + " and this is " + now
	}
	return v.OK, v.Reason
}

// probe runs the checks of Verify. The reason is empty when all of them
// held; the error says that they could not run.
func probe(ctx context.Context) (reason string, err error) {
	root, err := os.MkdirTemp("", "confine-verify-")
	if err != nil {
		return "", mcpkit.Errorf(mcpkit.Internal, "confine: verify: %v", err)
	}
	defer os.RemoveAll(root)
	if root, err = resolvePath(root); err != nil {
		return "", err
	}
	allowed := filepath.Join(root, "allowed")
	writable := filepath.Join(root, "writable")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{allowed, writable, outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", mcpkit.Errorf(mcpkit.Internal, "confine: verify: %v", err)
		}
	}
	inside := filepath.Join(allowed, "inside.txt")
	canary := filepath.Join(outside, "canary.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		return "", mcpkit.Errorf(mcpkit.Internal, "confine: verify: %v", err)
	}
	if err := os.WriteFile(canary, []byte(verifyCanary), 0o600); err != nil {
		return "", mcpkit.Errorf(mcpkit.Internal, "confine: verify: %v", err)
	}
	profile := Profile{ReadOnly: []string{allowed}, ReadWrite: []string{writable}}
	const write = `echo written > "$0"`

	out, failed, err := attempt(ctx, profile, "/bin/cat", inside)
	if err != nil {
		return "", err
	}
	if failed != nil || out != "inside" {
		return "an allowed read did not succeed", nil
	}

	out, failed, err = attempt(ctx, profile, "/bin/cat", canary)
	if err != nil {
		return "", err
	}
	if failed == nil || strings.Contains(out, verifyCanary) {
		return "a read outside the profile succeeded", nil
	}

	escaped := filepath.Join(outside, "written.txt")
	if _, _, err = attempt(ctx, profile, "/bin/sh", "-c", write, escaped); err != nil {
		return "", err
	}
	if _, err := os.Lstat(escaped); err == nil {
		return "a write outside the profile succeeded", nil
	}

	refused := filepath.Join(allowed, "written.txt")
	if _, _, err = attempt(ctx, profile, "/bin/sh", "-c", write, refused); err != nil {
		return "", err
	}
	if _, err := os.Lstat(refused); err == nil {
		return "a write to a read-only path succeeded", nil
	}

	kept := filepath.Join(writable, "written.txt")
	if _, failed, err = attempt(ctx, profile, "/bin/sh", "-c", write, kept); err != nil {
		return "", err
	}
	if _, err := os.Lstat(kept); failed != nil || err != nil {
		return "an allowed write did not succeed", nil
	}
	return "", nil
}

// attempt runs one confined command. failed is how the command ended; err
// says that it could not be started.
func attempt(ctx context.Context, profile Profile, name string, args ...string) (out string, failed, err error) {
	cmd, err := Command(ctx, profile, name, args...)
	if err != nil {
		return "", nil, err
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	failed = cmd.Wait()
	return output.String(), failed, nil
}
