// Copyright 2026 The fastexec Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux && (amd64 || arm64)

package fastexec

import (
	"encoding/binary"
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// cloneArgs is the clone3(2) argument structure (struct clone_args in
// linux/sched.h). The layout must match the kernel ABI exactly.
type cloneArgs struct {
	flags      uint64 // flags bit mask
	pidFD      uint64 // where to store PID file descriptor (int *)
	childTID   uint64 // where to store child TID, in child's memory (pid_t *)
	parentTID  uint64 // where to store child TID, in parent's memory (pid_t *)
	exitSignal uint64 // signal to deliver to parent on child termination
	stack      uint64 // pointer to lowest byte of stack
	stackSize  uint64 // size of stack
	tls        uint64 // location of new TLS
	setTID     uint64 // pointer to a pid_t array (since Linux 5.5)
	setTIDSize uint64 // number of elements in setTID (since Linux 5.5)
	cgroup     uint64 // file descriptor for target cgroup of child (since Linux 5.7)
}

// childState is the fixed-layout block shared with the vfork child. The
// assembly in fastexec_linux_{amd64,arm64}.s addresses these fields by
// byte offset; any layout change must be mirrored there.
//
// Because the child is created with CLONE_VFORK|CLONE_VM, the parent
// thread is suspended until the child calls execve or exits, so the
// child's write to errno is race-free and visible when clone3Spawn
// returns in the parent.
type childState struct {
	errno       uint64    // 0: setup/execve errno written by the child on failure
	dir         *byte     // 8: chdir(2) target, or nil
	path        *byte     // 16: execve(2) path
	argv        **byte    // 24: NULL-terminated argv vector
	envp        **byte    // 32: NULL-terminated envp vector
	fds         [3]uint64 // 40: stdio source fds (>= 3) dup3'd onto 0, 1, 2
	sigmask     uint64    // 64: original signal mask, restored just before execve when restoreMask != 0
	pipefd      uint64    // 72: if non-zero, fd the child also writes errno to on failure
	restoreMask uint64    // 80: non-zero if the child must restore sigmask (mask-dance fallback only)
}

// clone3Spawn issues clone3(2) with the given arguments and, in the
// child, performs stdio wiring, chdir, signal-mask restoration, and
// execve entirely in assembly on the private child stack. It returns in
// the parent only.
//
//go:noescape
func clone3Spawn(cargs *cloneArgs, size uintptr, child *childState) (pid, errno uintptr)

// cloneSpawn is the clone(2) equivalent of clone3Spawn, used when clone3
// is rejected (Docker's default seccomp profile returns ENOSYS for
// clone3; some older profiles return EPERM). CLONE_PIDFD via clone(2)
// requires Linux 5.2+ and stores the pidfd through the parent_tid slot.
//
//go:noescape
func cloneSpawn(flags, stackTop uintptr, pidfd *int32, child *childState) (pid, errno uintptr)

// childRun is the shared assembly child body entered from clone3Spawn or
// cloneSpawn in the vfork child. It never returns and must not be called
// from Go.
func childRun()

// rawSigprocmask issues rt_sigprocmask(2) for the calling thread without
// any scheduler interaction or stack growth, so it is safe to call while
// the spawn window must remain free of preemption points.
//
//go:noescape
func rawSigprocmask(how uintptr, set, old *uint64) (errno uintptr)

// childStackSize is the size of the private child stack. The child only
// executes a handful of raw system calls, but the kernel may also push a
// signal frame, so keep a generous margin.
const childStackSize = 64 << 10

// spawnState carries all memory referenced by the kernel and the vfork
// child across a single spawn. It lives on the heap (via spawnPool) so
// every address handed to clone3 stays stable even if the parent
// goroutine's stack moves.
type spawnState struct {
	cargs cloneArgs
	child childState
	pidfd int32
	stack []byte
	cs    cstrs
}

// spawnPool recycles spawnState values, including their child stacks and
// C-string arenas, across spawns.
var spawnPool = sync.Pool{
	New: func() any {
		return &spawnState{stack: make([]byte, childStackSize)}
	},
}

// allSigs is a full signal mask used to block every signal around the
// clone3 window, mirroring the runtime's fork path.
var allSigs = ^uint64(0)

// sigSetMask is the SIG_SETMASK constant of rt_sigprocmask(2).
const sigSetMask = 2

// cloneFlags is the flag set shared by the clone3 and clone spawn paths.
const cloneFlags = unix.CLONE_VFORK | unix.CLONE_VM | unix.CLONE_PIDFD

// clone3Unavailable latches whether clone3 was observed to be rejected
// (typically by a seccomp policy), so later spawns go straight to the
// clone(2) fallback.
var clone3Unavailable atomic.Bool

// clearSighandUnavailable latches whether CLONE_CLEAR_SIGHAND was
// observed to be rejected (EINVAL from a pre-5.5 kernel), so later
// spawns go straight to the signal-mask fallback. The
// FASTEXEC_NO_CLEAR_SIGHAND environment variable forces the latch, for
// testing the fallback on kernels that support the flag.
var clearSighandUnavailable atomic.Bool

func init() {
	if os.Getenv("FASTEXEC_NO_CLEAR_SIGHAND") != "" {
		clearSighandUnavailable.Store(true)
	}
}

// errnoViaPipe reports whether exec failures must be reported through a
// pipe instead of the vfork-shared childState.
//
// With CLONE_VFORK|CLONE_VM the child's errno store is visible to the
// parent directly, which is the fastest possible channel. Some virtual
// kernels (notably OrbStack's, where even glibc's posix_spawn loses the
// child's error report) silently drop the address-space sharing, so a
// one-time probe spawns a path that cannot exist and checks whether the
// child's errno write became visible. If it did not, every subsequent
// spawn carries a CLOEXEC pipe for error reporting, exactly like os/exec.
var errnoViaPipe = sync.OnceValue(func() bool {
	null, err := devNull()
	if err != nil {
		return true
	}
	probe := &Cmd{Path: "/dev/null/fastexec-probe", Args: []string{"fastexec-probe"}, Env: []string{}}
	if perr := startProcess1(probe, [3]*os.File{null, null, null}, false); perr != nil {
		// The child's [unix.ENOTDIR] was observed: shared memory works.
		return false
	}
	// The spawn "succeeded" even though the path cannot exist: the
	// child's write was lost. Reap the probe child and switch modes.
	var ps ProcessState
	probe.Process.wait(&ps) //nolint:errcheck // probe child is discarded either way
	probe.Process.release()
	return true
})

// startProcess spawns c.Path, filling in c.Process on success. files
// holds the resolved stdin/stdout/stderr files.
func startProcess(c *Cmd, files [3]*os.File) error {
	return startProcess1(c, files, errnoViaPipe())
}

// startProcess1 implements startProcess with an explicit error-reporting
// mode so the one-time probe can bypass the mode decision.
func startProcess1(c *Cmd, files [3]*os.File, usePipe bool) error {
	st := spawnPool.Get().(*spawnState)
	defer spawnPool.Put(st)

	env := c.Env
	if env == nil {
		env = defaultEnv()
	}
	pathp, dirp, argvp, envp, err := st.cs.build(c.Path, c.Dir, c.argv(), env)
	if err != nil {
		return &Error{Name: c.Path, Err: err}
	}

	// Stdio sources already at fd >= 3 (the /dev/null singleton, os.Pipe
	// ends, any normal file) are handed to the child as-is: the child's
	// dup3(src, 0..2) cannot collide because every source sits above the
	// target range. Non-CLOEXEC sources then appear in the child at their
	// original number, exactly as with os/exec. Only sources at fds 0-2
	// (a caller passing os.Stdin et al) are duplicated up to >= 3 with
	// O_CLOEXEC set atomically, keeping the dup3 sequence collision-free;
	// those temporary descriptors close themselves at execve.
	var highs [3]int
	var dupped [3]bool
	for i, f := range files {
		fd := f.Fd()
		if fd >= 3 {
			highs[i] = int(fd)
			continue
		}
		h, ferr := unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 3)
		if ferr != nil {
			for j, d := range highs[:i] {
				if dupped[j] {
					unix.Close(d)
				}
			}
			return &Error{Name: c.Path, Err: os.NewSyscallError("fcntl", ferr)}
		}
		highs[i] = h
		dupped[i] = true
	}
	defer func() {
		for i, d := range highs {
			if dupped[i] {
				unix.Close(d)
			}
		}
	}()

	// In pipe mode, the child reports exec failure by writing the errno
	// through a CLOEXEC pipe; success is signaled by EOF when execve
	// closes the write end. The write end is kept at fd >= 3 so the
	// child's dup3 calls onto 0..2 cannot clobber it.
	var pipeR, pipeW int
	if usePipe {
		var pfd [2]int
		if perr := unix.Pipe2(pfd[:], unix.O_CLOEXEC); perr != nil {
			return &Error{Name: c.Path, Err: os.NewSyscallError("pipe2", perr)}
		}
		pipeR, pipeW = pfd[0], pfd[1]
		if pipeW < 3 {
			nfd, perr := unix.FcntlInt(uintptr(pipeW), unix.F_DUPFD_CLOEXEC, 3)
			unix.Close(pipeW)
			if perr != nil {
				unix.Close(pipeR)
				return &Error{Name: c.Path, Err: os.NewSyscallError("fcntl", perr)}
			}
			pipeW = nfd
		}
	}

	st.child = childState{
		dir:  dirp,
		path: pathp,
		argv: argvp,
		envp: envp,
		fds:  [3]uint64{uint64(highs[0]), uint64(highs[1]), uint64(highs[2])},
	}
	if usePipe {
		st.child.pipefd = uint64(pipeW)
	}
	pid, errno := rawSpawn(st)
	runtime.KeepAlive(files)
	if errno != 0 {
		if usePipe {
			unix.Close(pipeR)
			unix.Close(pipeW)
		}
		if errno == unix.ENOSYS || errno == unix.EINVAL {
			return &Error{Name: c.Path, Err: errors.New("clone3/clone(CLONE_PIDFD) unavailable: fastexec requires Linux 5.4+ and a seccomp policy permitting them")}
		}
		return &Error{Name: c.Path, Err: os.NewSyscallError("clone", errno)}
	}

	if usePipe {
		unix.Close(pipeW)
		childErrno := readChildErrno(pipeR)
		unix.Close(pipeR)
		if childErrno != 0 {
			reapAbandoned(int(st.pidfd))
			unix.Close(int(st.pidfd))
			return &Error{Name: c.Path, Err: childErrno}
		}
	} else if st.child.errno != 0 {
		// The child failed before or at execve and has already exited;
		// reap it so no zombie is left behind.
		reapAbandoned(int(st.pidfd))
		unix.Close(int(st.pidfd))
		return &Error{Name: c.Path, Err: unix.Errno(st.child.errno)}
	}
	c.Process.Pid = int(pid)
	c.Process.pidfd = int(st.pidfd)
	return nil
}

// rawSpawn issues the clone3(2)/clone(2) call described by st.child and
// st.stack, handling the CLONE_CLEAR_SIGHAND and clone3 availability
// latches. The caller must have populated st.child (rawSpawn owns the
// sigmask and restoreMask fields) and keeps st reachable across the
// call.
//
// On the fast path (clone3 accepting CLONE_CLEAR_SIGHAND, Linux 5.5+)
// the kernel resets every signal disposition to SIG_DFL atomically
// inside the child, so no Go signal handler can ever run in the shared
// address space and no signal masking or thread pinning is needed at
// all. Kernels that reject the flag (EINVAL, pre-5.5) latch
// clearSighandUnavailable and retry with the fallback: block every
// signal on this thread for the vfork window and have the child restore
// the original mask immediately before execve. Between mask and restore
// only nosplit assembly runs, so the window contains no preemption
// points. The clone(2) seccomp fallback cannot express
// CLONE_CLEAR_SIGHAND and always uses the mask dance.
func rawSpawn(st *spawnState) (pid uintptr, err unix.Errno) {
	st.pidfd = -1
	stackBase := uintptr(unsafe.Pointer(&st.stack[0]))
	st.cargs = cloneArgs{
		flags:      cloneFlags,
		pidFD:      uint64(uintptr(unsafe.Pointer(&st.pidfd))),
		exitSignal: uint64(unix.SIGCHLD),
		stack:      uint64(stackBase),
		stackSize:  uint64(len(st.stack)),
	}
	// clone(2) takes the initial stack pointer directly and encodes the
	// exit signal in the flag word.
	stackTop := stackBase + uintptr(len(st.stack))
	rawFlags := uintptr(cloneFlags) | uintptr(unix.SIGCHLD)

	tryClone3 := !clone3Unavailable.Load()
	useClear := tryClone3 && !clearSighandUnavailable.Load()
	st.child.restoreMask = 0
	masked := false
	if useClear {
		st.cargs.flags |= unix.CLONE_CLEAR_SIGHAND
	} else {
		if e := st.beginMask(); e != 0 {
			return 0, e
		}
		masked = true
	}

	var e uintptr
	fellBack := !tryClone3
	if tryClone3 {
		pid, e = clone3Spawn(&st.cargs, unsafe.Sizeof(st.cargs), &st.child)
		if useClear && unix.Errno(e) == unix.EINVAL {
			// The kernel predates CLONE_CLEAR_SIGHAND (5.4): latch it
			// off and retry this and every later spawn with the
			// signal-mask fallback.
			clearSighandUnavailable.Store(true)
			useClear = false
			st.cargs.flags = cloneFlags
			if me := st.beginMask(); me != 0 {
				return 0, me
			}
			masked = true
			pid, e = clone3Spawn(&st.cargs, unsafe.Sizeof(st.cargs), &st.child)
		}
		if unix.Errno(e) == unix.ENOSYS || unix.Errno(e) == unix.EPERM {
			fellBack = true
			if !masked {
				// clone(2) cannot clear dispositions atomically.
				if me := st.beginMask(); me != 0 {
					return 0, me
				}
				masked = true
			}
			pid, e = cloneSpawn(rawFlags, stackTop, &st.pidfd, &st.child)
		}
	} else {
		pid, e = cloneSpawn(rawFlags, stackTop, &st.pidfd, &st.child)
	}
	if masked {
		rawSigprocmask(sigSetMask, &st.child.sigmask, nil)
		runtime.UnlockOSThread()
	}
	runtime.KeepAlive(st)
	if e != 0 {
		return 0, unix.Errno(e)
	}
	if fellBack && tryClone3 {
		clone3Unavailable.Store(true)
	}
	return pid, 0
}

// beginMask blocks every signal on the calling thread ahead of a spawn
// that cannot use CLONE_CLEAR_SIGHAND, saving the previous mask into
// st.child.sigmask for the child (and later the parent) to restore, and
// marks the child to perform that restore just before execve.
func (st *spawnState) beginMask() unix.Errno {
	runtime.LockOSThread()
	st.child.restoreMask = 1
	if e := rawSigprocmask(sigSetMask, &allSigs, &st.child.sigmask); e != 0 {
		runtime.UnlockOSThread()
		return unix.Errno(e)
	}
	return 0
}

// readChildErrno reads the child's exec-failure report from the error
// pipe. EOF means execve succeeded (the CLOEXEC write end was closed by
// the kernel); an 8-byte payload carries the child's errno.
func readChildErrno(fd int) unix.Errno {
	var buf [8]byte
	for {
		n, err := unix.Read(fd, buf[:])
		switch {
		case err == unix.EINTR:
			continue
		case err != nil || n == 0:
			return 0
		case n == len(buf):
			return unix.Errno(binary.NativeEndian.Uint64(buf[:]))
		default:
			return unix.EIO
		}
	}
}

// reapAbandoned reaps a child known to have exited already.
func reapAbandoned(pidfd int) {
	var si siginfo
	for {
		_, _, e := unix.Syscall6(unix.SYS_WAITID, unix.P_PIDFD, uintptr(pidfd),
			uintptr(unsafe.Pointer(&si)), uintptr(unix.WEXITED), 0, 0)
		if e != unix.EINTR {
			return
		}
	}
}

// Process represents a running process created by [Cmd.Start].
//
// All signaling goes through the process's pidfd, so a Process can never
// signal an unrelated process even if the kernel recycles the PID.
type Process struct {
	// Pid is the process ID of the child.
	Pid int

	pidfd    int
	mu       sync.Mutex
	released bool
}

// siginfo mirrors the kernel's siginfo_t (128 bytes) as filled in by
// waitid(2) for the CLD_* codes on 64-bit architectures.
type siginfo struct {
	signo  int32
	errno  int32
	code   int32
	_      int32
	pid    int32
	uid    uint32
	status int32
	_      [100]byte
}

// Linux CLD_* si_code values reported by waitid(2).
const (
	cldExited    = 1
	cldKilled    = 2
	cldDumped    = 3
	cldTrapped   = 4
	cldStopped   = 5
	cldContinued = 6
)

// waitStatus converts the siginfo into the wait(2)-style encoding used by
// [unix.WaitStatus].
func (si *siginfo) waitStatus() unix.WaitStatus {
	switch si.code {
	case cldExited:
		return unix.WaitStatus(si.status&0xff) << 8
	case cldKilled:
		return unix.WaitStatus(si.status & 0x7f)
	case cldDumped:
		return unix.WaitStatus(si.status&0x7f | 0x80)
	case cldTrapped, cldStopped:
		return unix.WaitStatus(si.status&0xff)<<8 | 0x7f
	case cldContinued:
		return unix.WaitStatus(0xffff)
	default:
		return unix.WaitStatus(si.status)
	}
}

// wait blocks until the process exits, storing its final state into
// ps. It waits on the pidfd, so it never races with PID reuse.
func (p *Process) wait(ps *ProcessState) error {
	var si siginfo
	for {
		_, _, e := unix.Syscall6(unix.SYS_WAITID, unix.P_PIDFD, uintptr(p.pidfd),
			uintptr(unsafe.Pointer(&si)), uintptr(unix.WEXITED),
			uintptr(unsafe.Pointer(&ps.rusage)), 0)
		if e == 0 {
			break
		}
		if e != unix.EINTR {
			return os.NewSyscallError("waitid", e)
		}
	}
	ps.pid = int(si.pid)
	ps.status = si.waitStatus()
	return nil
}

// Signal sends sig to the process through its pidfd.
func (p *Process) Signal(sig unix.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return ErrProcessDone
	}
	if err := unix.PidfdSendSignal(p.pidfd, unix.Signal(sig), nil, 0); err != nil {
		if err == unix.ESRCH {
			return ErrProcessDone
		}
		return os.NewSyscallError("pidfd_send_signal", err)
	}
	return nil
}

// Kill causes the process to exit immediately ([unix.SIGKILL]).
func (p *Process) Kill() error {
	return p.Signal(unix.SIGKILL)
}

// reset returns the Process to its pre-start state for Cmd reuse; the
// embedded mutex is retained.
func (p *Process) reset() {
	p.Pid = 0
	p.pidfd = -1
	p.released = false
}

// release closes the pidfd. It must only be called after wait has reaped
// the process and any signaling goroutines have finished.
func (p *Process) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.released {
		p.released = true
		unix.Close(p.pidfd)
		p.pidfd = -1
	}
}
