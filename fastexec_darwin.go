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

// syscall_rawSyscall6 is the raw variant of syscall_syscall6: no
// entersyscall/exitsyscall bookkeeping, saving ~100ns each way. Only
// used for libSystem calls that complete in bounded userspace time (the
// posix_spawnattr and file-actions bookkeeping family); posix_spawn
// itself blocks in the kernel and stays on syscall_syscall6.
//
//go:linkname syscall_rawSyscall6 syscall.rawSyscall6
func syscall_rawSyscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err unix.Errno)

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

// libcCallRaw is libcCall without scheduler interaction, for the
// non-blocking posix_spawn bookkeeping calls.
func libcCallRaw(fn, a1, a2, a3, a4, a5, a6 uintptr) unix.Errno {
	r1, _, _ := syscall_rawSyscall6(fn, a1, a2, a3, a4, a5, a6)
	return unix.Errno(r1)
}

// spawnState carries the reusable per-spawn resources: the C-string
// arena, the pooled posix_spawn attribute (whose settings are identical
// for every spawn, so it is initialized once per spawnState), and the
// opaque file-actions object (initialized and destroyed per spawn,
// since file actions accumulate).
type spawnState struct {
	cs      cstrs
	attr    uintptr    // pooled posix_spawnattr_t (void *)
	attrErr unix.Errno // non-zero if the one-time attr setup failed
	facts   uintptr    // posix_spawn_file_actions_t (void *)
	pid     int32
}

// spawnPool recycles spawnState values, including their pooled spawn
// attributes, across spawns.
var spawnPool = sync.Pool{
	New: func() any {
		st := &spawnState{}
		st.initAttr()
		return st
	},
}

// initAttr performs the one-time posix_spawnattr_t setup shared by every
// spawn: all signal dispositions reset to their defaults, an empty
// signal mask, and POSIX_SPAWN_CLOEXEC_DEFAULT. The C allocation is
// destroyed by a runtime.AddCleanup when the spawnState is collected,
// which by construction means it has left the pool, so pool churn can
// neither leak nor double-free it.
func (st *spawnState) initAttr() {
	if e := libcCallRaw(
		libc_posix_spawnattr_init_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), 0, 0, 0, 0, 0); e != 0 {
		st.attrErr = e
		return
	}
	sigFull := ^uint32(0)
	sigNone := uint32(0)
	if e := libcCallRaw(
		libc_posix_spawnattr_setsigdefault_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), uintptr(unsafe.Pointer(&sigFull)), 0, 0, 0, 0); e != 0 {
		st.destroyAttrOnErr(e)
		return
	}
	if e := libcCallRaw(
		libc_posix_spawnattr_setsigmask_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), uintptr(unsafe.Pointer(&sigNone)), 0, 0, 0, 0); e != 0 {
		st.destroyAttrOnErr(e)
		return
	}
	if e := libcCallRaw(
		libc_posix_spawnattr_setflags_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)),
		posixSpawnSetSigDef|posixSpawnSetSigMask|posixSpawnCloexecDefault, 0, 0, 0, 0); e != 0 {
		st.destroyAttrOnErr(e)
		return
	}
	runtime.AddCleanup(st, func(attr uintptr) {
		libcCallRaw(
			libc_posix_spawnattr_destroy_trampoline_addr,
			uintptr(unsafe.Pointer(&attr)), 0, 0, 0, 0, 0)
	}, st.attr)
}

// destroyAttrOnErr releases a partially configured attribute and
// records why the setup failed; every spawn using this spawnState then
// reports that error.
func (st *spawnState) destroyAttrOnErr(e unix.Errno) {
	libcCallRaw(
		libc_posix_spawnattr_destroy_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), 0, 0, 0, 0, 0)
	st.attr = 0
	st.attrErr = e
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

	// Stdio sources already at fd >= 3 (the /dev/null singleton, os.Pipe
	// ends, any normal file) feed the dup2 file actions as-is: with every
	// source above the target range 0..2 the actions cannot collide, and
	// POSIX_SPAWN_CLOEXEC_DEFAULT still closes everything that was not
	// explicitly mapped. Only sources at fds 0-2 (a caller passing
	// [os.Stdin] et al) are duplicated up to >= 3 with O_CLOEXEC set
	// atomically, keeping the actions collision-free.
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
			return nil, &Error{Name: c.Path, Err: os.NewSyscallError("fcntl", ferr)}
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

	// The pooled attribute already carries SETSIGDEF, SETSIGMASK, and
	// CLOEXEC_DEFAULT; only the per-spawn file actions are built here.
	if st.attrErr != 0 {
		return nil, &Error{Name: c.Path, Err: os.NewSyscallError("posix_spawnattr_init", st.attrErr)}
	}

	st.facts = 0
	if e := libcCallRaw(
		libc_posix_spawn_file_actions_init_trampoline_addr,
		uintptr(unsafe.Pointer(&st.facts)), 0, 0, 0, 0, 0); e != 0 {
		return nil, &Error{Name: c.Path, Err: os.NewSyscallError("posix_spawn_file_actions_init", e)}
	}
	defer libcCallRaw(
		libc_posix_spawn_file_actions_destroy_trampoline_addr,
		uintptr(unsafe.Pointer(&st.facts)), 0, 0, 0, 0, 0)

	for i, h := range highs {
		if e := libcCallRaw(
			libc_posix_spawn_file_actions_adddup2_trampoline_addr,
			uintptr(unsafe.Pointer(&st.facts)), uintptr(h), uintptr(i), 0, 0, 0); e != 0 {
			return nil, &Error{Name: c.Path, Err: os.NewSyscallError("posix_spawn_file_actions_adddup2", e)}
		}
	}
	if dirp != nil {
		if e := libcCallRaw(
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
