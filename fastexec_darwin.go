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

//go:build darwin

package fastexec

import (
	"os"
	"runtime"
	"sync"
	_ "syscall" // for go:linkname
	"unsafe"
	_ "unsafe" // for go:linkname

	"golang.org/x/sys/unix"
)

// posix_spawn attribute flags from <sys/spawn.h>.
const (
	posixSpawnSetSigDef      = 0x0004 // POSIX_SPAWN_SETSIGDEF
	posixSpawnSetSigMask     = 0x0008 // POSIX_SPAWN_SETSIGMASK
	posixSpawnCloexecDefault = 0x4000 // POSIX_SPAWN_CLOEXEC_DEFAULT (Apple-specific)
)

// syscall_syscall6 is the Go runtime's libc call gate (via the syscall
// package), the same mechanism golang.org/x/sys/unix uses. Calling
// libSystem through it costs a normal entersyscall/exitsyscall, with none
// of cgo's thread and stack switching overhead.
//
//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err unix.Errno)

// Lazily bound libSystem symbols; the trampolines live in
// fastexec_darwin.s.
//
//go:cgo_import_dynamic libc_posix_spawn posix_spawn "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_init posix_spawnattr_init "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_destroy posix_spawnattr_destroy "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_setflags posix_spawnattr_setflags "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_setsigdefault posix_spawnattr_setsigdefault "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_setsigmask posix_spawnattr_setsigmask "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_init posix_spawn_file_actions_init "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_destroy posix_spawn_file_actions_destroy "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_adddup2 posix_spawn_file_actions_adddup2 "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_addchdir_np posix_spawn_file_actions_addchdir_np "/usr/lib/libSystem.B.dylib"

var (
	libc_posix_spawn_trampoline_addr                          uintptr
	libc_posix_spawnattr_init_trampoline_addr                 uintptr
	libc_posix_spawnattr_destroy_trampoline_addr              uintptr
	libc_posix_spawnattr_setflags_trampoline_addr             uintptr
	libc_posix_spawnattr_setsigdefault_trampoline_addr        uintptr
	libc_posix_spawnattr_setsigmask_trampoline_addr           uintptr
	libc_posix_spawn_file_actions_init_trampoline_addr        uintptr
	libc_posix_spawn_file_actions_destroy_trampoline_addr     uintptr
	libc_posix_spawn_file_actions_adddup2_trampoline_addr     uintptr
	libc_posix_spawn_file_actions_addchdir_np_trampoline_addr uintptr
)

// libcCall invoke a libSystem function that reports failure by
// returning an errno value (the posix_spawn family convention).
func libcCall(fn, a1, a2, a3, a4, a5, a6 uintptr) unix.Errno {
	r1, _, _ := syscall_syscall6(fn, a1, a2, a3, a4, a5, a6)
	return unix.Errno(r1)
}

// spawnState carries the reusable per-spawn resources: the C-string arena
// and the opaque posix_spawn attribute and file-action objects (which are
// initialized and destroyed per spawn, since file actions accumulate).
type spawnState struct {
	cs      cstrs
	attr    uintptr // posix_spawnattr_t (void *)
	facts   uintptr // posix_spawn_file_actions_t (void *)
	sigFull uint32  // sigset_t: all signals
	sigNone uint32  // sigset_t: empty
	pid     int32
}

// spawnPool recycles spawnState values across spawns.
var spawnPool = sync.Pool{
	New: func() any { return &spawnState{} },
}

// startProcess spawns c.Path via posix_spawn(2) and returns the resulting
// process handle. files holds the resolved stdin/stdout/stderr files.
//
// POSIX_SPAWN_CLOEXEC_DEFAULT guarantees that only the three dup2'd stdio
// descriptors survive into the child, so no descriptor can leak
// regardless of concurrent descriptor creation elsewhere in the process,
// and [syscall.ForkLock] is never taken.
func startProcess(c *Cmd, files [3]*os.File) (*Process, error) {
	st := spawnPool.Get().(*spawnState)
	defer spawnPool.Put(st)

	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	pathp, dirp, argvp, envp, err := st.cs.build(c.Path, c.Dir, c.argv(), env)
	if err != nil {
		return nil, &Error{Name: c.Path, Err: err}
	}

	// Duplicate every stdio source to a fresh descriptor >= 3 with
	// O_CLOEXEC set atomically, making the dup2 file actions immune to
	// source/target overlap (for example when the caller passes
	// [os.Stdout] as the child's stderr).
	var highs [3]int
	for i, f := range files {
		h, ferr := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 3)
		if ferr != nil {
			for _, d := range highs[:i] {
				unix.Close(d)
			}
			return nil, &Error{Name: c.Path, Err: os.NewSyscallError("fcntl", ferr)}
		}
		highs[i] = h
	}
	defer func() {
		for _, d := range highs {
			unix.Close(d)
		}
	}()

	st.attr = 0
	if e := libcCall(
		libc_posix_spawnattr_init_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), 0, 0, 0, 0, 0); e != 0 {
		return nil, &Error{Name: c.Path, Err: os.NewSyscallError("posix_spawnattr_init", e)}
	}
	defer libcCall(
		libc_posix_spawnattr_destroy_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), 0, 0, 0, 0, 0)

	// Reset every signal disposition to its default and start the child
	// with an empty signal mask, so the Go runtime's handlers and mask
	// never leak into the child.
	st.sigFull = ^uint32(0)
	st.sigNone = 0
	if e := libcCall(
		libc_posix_spawnattr_setsigdefault_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), uintptr(unsafe.Pointer(&st.sigFull)), 0, 0, 0, 0); e != 0 {
		return nil, &Error{Name: c.Path, Err: os.NewSyscallError("posix_spawnattr_setsigdefault", e)}
	}
	if e := libcCall(
		libc_posix_spawnattr_setsigmask_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), uintptr(unsafe.Pointer(&st.sigNone)), 0, 0, 0, 0); e != 0 {
		return nil, &Error{Name: c.Path, Err: os.NewSyscallError("posix_spawnattr_setsigmask", e)}
	}
	if e := libcCall(
		libc_posix_spawnattr_setflags_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)),
		posixSpawnSetSigDef|posixSpawnSetSigMask|posixSpawnCloexecDefault, 0, 0, 0, 0); e != 0 {
		return nil, &Error{Name: c.Path, Err: os.NewSyscallError("posix_spawnattr_setflags", e)}
	}

	st.facts = 0
	if e := libcCall(
		libc_posix_spawn_file_actions_init_trampoline_addr,
		uintptr(unsafe.Pointer(&st.facts)), 0, 0, 0, 0, 0); e != 0 {
		return nil, &Error{Name: c.Path, Err: os.NewSyscallError("posix_spawn_file_actions_init", e)}
	}
	defer libcCall(
		libc_posix_spawn_file_actions_destroy_trampoline_addr,
		uintptr(unsafe.Pointer(&st.facts)), 0, 0, 0, 0, 0)

	for i, h := range highs {
		if e := libcCall(
			libc_posix_spawn_file_actions_adddup2_trampoline_addr,
			uintptr(unsafe.Pointer(&st.facts)), uintptr(h), uintptr(i), 0, 0, 0); e != 0 {
			return nil, &Error{Name: c.Path, Err: os.NewSyscallError("posix_spawn_file_actions_adddup2", e)}
		}
	}
	if dirp != nil {
		if e := libcCall(
			libc_posix_spawn_file_actions_addchdir_np_trampoline_addr,
			uintptr(unsafe.Pointer(&st.facts)), uintptr(unsafe.Pointer(dirp)), 0, 0, 0, 0); e != 0 {
			return nil, &Error{Name: c.Path, Err: os.NewSyscallError("posix_spawn_file_actions_addchdir_np", e)}
		}
	}

	st.pid = 0
	e := libcCall(
		libc_posix_spawn_trampoline_addr,
		uintptr(unsafe.Pointer(&st.pid)), uintptr(unsafe.Pointer(pathp)),
		uintptr(unsafe.Pointer(&st.facts)), uintptr(unsafe.Pointer(&st.attr)),
		uintptr(unsafe.Pointer(argvp)), uintptr(unsafe.Pointer(envp)),
	)
	runtime.KeepAlive(st)
	runtime.KeepAlive(files)
	if e != 0 {
		return nil, &Error{Name: c.Path, Err: e}
	}
	return &Process{Pid: int(st.pid)}, nil
}

// Process represents a running process created by [Cmd.Start].
//
// Darwin has no pidfd equivalent, so signaling uses the PID, guarded so
// that no signal is sent after the process has been reaped by wait.
type Process struct {
	// Pid is the process ID of the child.
	Pid int

	sigMu sync.RWMutex
	done  bool
}

// wait blocks until the process exits and returns its final state.
func (p *Process) wait() (*ProcessState, error) {
	ps := &ProcessState{pid: p.Pid}
	for {
		wpid, err := unix.Wait4(p.Pid, &ps.status, 0, &ps.rusage)
		if err == nil {
			_ = wpid
			break
		}
		if err != unix.EINTR {
			return nil, os.NewSyscallError("wait4", err)
		}
	}
	p.sigMu.Lock()
	p.done = true
	p.sigMu.Unlock()
	return ps, nil
}

// Signal sends sig to the process. It returns [ErrProcessDone] if the
// process has already been reaped.
func (p *Process) Signal(sig unix.Signal) error {
	p.sigMu.RLock()
	defer p.sigMu.RUnlock()
	if p.done {
		return ErrProcessDone
	}
	if err := unix.Kill(p.Pid, sig); err != nil {
		if err == unix.ESRCH {
			return ErrProcessDone
		}
		return os.NewSyscallError("kill", err)
	}
	return nil
}

// Kill causes the process to exit immediately ([unix.SIGKILL]).
func (p *Process) Kill() error {
	return p.Signal(unix.SIGKILL)
}

// release is a no-op on Darwin; there is no handle beyond the PID.
func (p *Process) release() {}
