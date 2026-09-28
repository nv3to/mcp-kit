//go:build linux

package confine

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"
)

// seccompArches are what the filter of the forwarder knows of each
// architecture: its number in the data the kernel filters, the number of the
// call that installs a filter, and the numbers of the calls it denies. Those
// are listen, and io_uring_setup, io_uring_enter and io_uring_register,
// because a ring can listen too.
var seccompArches = map[string]struct {
	audit   uint32
	seccomp uintptr
	denied  []uint32
}{
	"amd64": {audit: 0xc000003e, seccomp: 317, denied: []uint32{50, 425, 426, 427}},
	"arm64": {audit: 0xc00000b7, seccomp: 277, denied: []uint32{201, 425, 426, 427}},
}

// The parts of a seccomp filter: instructions of classic BPF, the actions
// of a filter, and the arguments that install one.
const (
	bpfLoad = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeq  = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJge  = 0x35 // BPF_JMP | BPF_JGE | BPF_K
	bpfRet  = 0x06 // BPF_RET | BPF_K

	seccompAllow = 0x7fff0000
	seccompErrno = 0x00050000
	seccompKill  = 0x80000000 // the whole process

	// seccompNr and seccompArch are where the number of the call and the
	// architecture lie in the data the kernel filters.
	seccompNr   = 0
	seccompArch = 4

	// x32 is the bit that marks a call of the x32 ABI on amd64.
	x32 = 0x40000000

	seccompSetModeFilter = 1
	seccompFilterTsync   = 1
	prSetNoNewPrivs      = 38
)

type sockFilter struct {
	code uint16
	jt   uint8
	jf   uint8
	k    uint32
}

type sockFprog struct {
	len    uint16
	filter *sockFilter
}

// forwardable says why this system cannot run the forwarder, or nil.
func forwardable() error {
	if _, ok := seccompArches[runtime.GOARCH]; !ok {
		return fmt.Errorf("a proxy is not supported on linux/%s, where the forwarder cannot keep the command from listening", runtime.GOARCH)
	}
	return nil
}

// forward runs the forwarder inside the sandbox, before the command. Its
// arguments are the address of the proxy, the unix socket of the relay and
// the command. It listens on the address of the proxy, on the loopback of
// the command's own network namespace, and carries every connection to the
// socket. Then it keeps the command from listening, starts it, and ends as
// it ends, so that neither outlives the other.
func forward(args []string) int {
	if len(args) < 3 || os.Getenv(Marker) == "" {
		fmt.Fprintln(os.Stderr, "confine: the forwarder runs inside a sandbox that confine started")
		return forwardFailed
	}
	addr, socket, argv := args[0], args[1], args[2:]
	l, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "confine: the forwarder: %v\n", err)
		return forwardFailed
	}
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				out, err := net.Dial("unix", socket)
				if err != nil {
					in.Close()
					return
				}
				join(in, out)
			}()
		}
	}()
	if err := denyListen(); err != nil {
		fmt.Fprintf(os.Stderr, "confine: the forwarder: %v\n", err)
		return forwardFailed
	}

	// The command inherits the environment and the working directory.
	cmd := &exec.Cmd{Path: argv[0], Args: argv, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "confine: %v\n", err)
		if errors.Is(err, fs.ErrNotExist) {
			return 127
		}
		return 126
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		// As bubblewrap reports a command that a signal ended.
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "confine: %v\n", err)
	return forwardFailed
}

// denyListen installs a seccomp filter on every thread of this process, and
// so on the command it starts: listen fails with EPERM. A call of another
// architecture or of the x32 ABI, which have calls of other numbers, ends
// the process.
func denyListen() error {
	if err := forwardable(); err != nil {
		return err
	}
	arch := seccompArches[runtime.GOARCH]
	prog := seccompProgram(arch.audit, arch.denied)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("no_new_privs: %v", errno)
	}
	fprog := sockFprog{len: uint16(len(prog)), filter: &prog[0]}
	// With TSYNC the call returns the thread that could not take the filter.
	thread, _, errno := syscall.RawSyscall(arch.seccomp, seccompSetModeFilter, seccompFilterTsync, uintptr(unsafe.Pointer(&fprog)))
	if errno != 0 {
		return fmt.Errorf("seccomp: %v", errno)
	}
	if thread != 0 {
		return fmt.Errorf("seccomp: thread %d cannot take the filter", thread)
	}
	return nil
}

// seccompProgram is the filter of denyListen. A jump counts from the next
// instruction.
func seccompProgram(audit uint32, denied []uint32) []sockFilter {
	n := uint8(len(denied))
	prog := []sockFilter{
		{code: bpfLoad, k: seccompArch},
		{code: bpfJeq, jf: n + 4, k: audit},
		{code: bpfLoad, k: seccompNr},
		{code: bpfJge, jt: n + 2, k: x32},
	}
	for i, nr := range denied {
		prog = append(prog, sockFilter{code: bpfJeq, jt: n - uint8(i), k: nr})
	}
	return append(prog,
		sockFilter{code: bpfRet, k: seccompAllow},
		sockFilter{code: bpfRet, k: seccompErrno | uint32(syscall.EPERM)},
		sockFilter{code: bpfRet, k: seccompKill},
	)
}
