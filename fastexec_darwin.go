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
		uintptr(unsafe.Pointer(&st.attr)), 0, 0, 0, 0, 0,
	); e != 0 {
		st.attrErr = e
		return
	}
	sigFull := ^uint32(0)
	sigNone := uint32(0)
	if e := libcCallRaw(
		libc_posix_spawnattr_setsigdefault_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), uintptr(unsafe.Pointer(&sigFull)), 0, 0, 0, 0,
	); e != 0 {
		st.destroyAttrOnErr(e)
		return
	}
	if e := libcCallRaw(
		libc_posix_spawnattr_setsigmask_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), uintptr(unsafe.Pointer(&sigNone)), 0, 0, 0, 0,
	); e != 0 {
		st.destroyAttrOnErr(e)
		return
	}
	if e := libcCallRaw(
		libc_posix_spawnattr_setflags_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)),
		posixSpawnSetSigDef|posixSpawnSetSigMask|posixSpawnCloexecDefault, 0, 0, 0, 0,
	); e != 0 {
		st.destroyAttrOnErr(e)
		return
	}
	runtime.AddCleanup(st, func(attr uintptr) {
		libcCallRaw(
			libc_posix_spawnattr_destroy_trampoline_addr,
			uintptr(unsafe.Pointer(&attr)), 0, 0, 0, 0, 0,
		)
	}, st.attr)
}

// destroyAttrOnErr releases a partially configured attribute and
// records why the setup failed; every spawn using this spawnState then
// reports that error.
func (st *spawnState) destroyAttrOnErr(e unix.Errno) {
	libcCallRaw(
		libc_posix_spawnattr_destroy_trampoline_addr,
		uintptr(unsafe.Pointer(&st.attr)), 0, 0, 0, 0, 0,
	)
	st.attr = 0
	st.attrErr = e
}

// startProcess spawns c.Path via posix_spawn(2), filling in c.Process
// on success. files holds the resolved stdin/stdout/stderr files.
//
// POSIX_SPAWN_CLOEXEC_DEFAULT guarantees that only the three dup2'd stdio
// descriptors survive into the child, so no descriptor can leak
// regardless of concurrent descriptor creation elsewhere in the process,
// and [syscall.ForkLock] is never taken.
func startProcess(c *Cmd, files [3]*os.File) error {
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
	pid, err := spawn1(st, c.Path, pathp, dirp, argvp, envp, files)
	if err != nil {
		return err
	}
	c.Process.Pid = pid
	return nil
}

// spawnRunner spawns the frozen s. With all-nil stdio (every stream
// resolved to the /dev/null singleton) the file actions frozen at
// NewRunner are used and the spawn is a single posix_spawn libc call;
// otherwise the general per-spawn file-actions path runs.
func spawnRunner(s *Runner, files [3]*os.File) (int, error) {
	st := spawnPool.Get().(*spawnState)
	defer spawnPool.Put(st)
	if st.attrErr != 0 {
		return 0, &Error{Name: s.path, Err: os.NewSyscallError("posix_spawnattr_init", st.attrErr)}
	}

	null, err := devNull()
	if err != nil {
		return 0, &Error{Name: s.path, Err: err}
	}
	if files[0] == null && files[1] == null && files[2] == null {
		st.pid = 0
		e := libcCall(
			libc_posix_spawn_trampoline_addr,
			uintptr(unsafe.Pointer(&st.pid)), uintptr(unsafe.Pointer(s.pathp)),
			uintptr(unsafe.Pointer(&s.os.nullFacts)), uintptr(unsafe.Pointer(&st.attr)),
			uintptr(unsafe.Pointer(s.argvp)), uintptr(unsafe.Pointer(s.envp)),
		)
		runtime.KeepAlive(s)
		if e != 0 {
			return 0, &Error{Name: s.path, Err: e}
		}
		return int(st.pid), nil
	}
	pid, err := spawn1(st, s.path, s.pathp, s.dirp, s.argvp, s.envp, files)
	runtime.KeepAlive(s)
	return pid, err
}

// startRunner spawns the frozen s, filling in p.
func startRunner(s *Runner, files [3]*os.File, p *Process) error {
	pid, err := spawnRunner(s, files)
	if err != nil {
		return err
	}
	p.Pid = pid
	return nil
}

// runRunner spawns the frozen s and reaps it inline, avoiding the
// Process handle so the whole cycle is allocation-free.
func runRunner(s *Runner, files [3]*os.File, ps *ProcessState) error {
	pid, err := spawnRunner(s, files)
	if err != nil {
		return err
	}
	ps.pid = pid
	for {
		if _, err := unix.Wait4(pid, &ps.status, 0, &ps.rusage); err == nil {
			break
		} else if err != unix.EINTR {
			return os.NewSyscallError("wait4", err)
		}
	}
	return nil
}

// spawn1 wires stdio into per-spawn file actions and issues the
// posix_spawn; it is the spawn core shared by the Cmd and the
// non-frozen Runner paths.
func spawn1(st *spawnState, name string, pathp, dirp *byte, argvp, envp **byte, files [3]*os.File) (int, error) {
	// The pooled attribute already carries SETSIGDEF, SETSIGMASK, and
	// CLOEXEC_DEFAULT; only the per-spawn file actions are built here.
	if st.attrErr != 0 {
		return 0, &Error{Name: name, Err: os.NewSyscallError("posix_spawnattr_init", st.attrErr)}
	}

	highs, dupped, err := prepStdio(files)
	if err != nil {
		return 0, &Error{Name: name, Err: err}
	}
	defer closeDupped(highs, dupped)

	st.facts = 0
	if e := libcCallRaw(libc_posix_spawn_file_actions_init_trampoline_addr,
		uintptr(unsafe.Pointer(&st.facts)), 0, 0, 0, 0, 0); e != 0 {
		return 0, &Error{Name: name, Err: os.NewSyscallError("posix_spawn_file_actions_init", e)}
	}
	defer libcCallRaw(libc_posix_spawn_file_actions_destroy_trampoline_addr,
		uintptr(unsafe.Pointer(&st.facts)), 0, 0, 0, 0, 0)

	for i, h := range highs {
		if e := libcCallRaw(libc_posix_spawn_file_actions_adddup2_trampoline_addr,
			uintptr(unsafe.Pointer(&st.facts)), uintptr(h), uintptr(i), 0, 0, 0); e != 0 {
			return 0, &Error{Name: name, Err: os.NewSyscallError("posix_spawn_file_actions_adddup2", e)}
		}
	}
	if dirp != nil {
		if e := libcCallRaw(libc_posix_spawn_file_actions_addchdir_np_trampoline_addr,
			uintptr(unsafe.Pointer(&st.facts)), uintptr(unsafe.Pointer(dirp)), 0, 0, 0, 0); e != 0 {
			return 0, &Error{Name: name, Err: os.NewSyscallError("posix_spawn_file_actions_addchdir_np", e)}
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
		return 0, &Error{Name: name, Err: e}
	}
	return int(st.pid), nil
}

// runnerOS carries the Darwin-specific frozen state of a Runner: a
// file-actions object prebuilt for the all-/dev/null stdio case (plus
// the frozen chdir, if any), so that path spawns with one libc call.
type runnerOS struct {
	nullFacts uintptr
}

// freeze prebuilds the nil-stdio file actions. The C allocation is
// destroyed by a runtime.AddCleanup when the Runner is collected;
// posix_spawn only reads the object, so concurrent spawns may share it.
func (s *Runner) freeze() error {
	null, err := devNull()
	if err != nil {
		return &Error{Name: s.path, Err: err}
	}
	nfd := uintptr(null.Fd())
	if e := libcCallRaw(libc_posix_spawn_file_actions_init_trampoline_addr,
		uintptr(unsafe.Pointer(&s.os.nullFacts)), 0, 0, 0, 0, 0); e != 0 {
		return &Error{Name: s.path, Err: os.NewSyscallError("posix_spawn_file_actions_init", e)}
	}
	for i := range uintptr(3) {
		if e := libcCallRaw(libc_posix_spawn_file_actions_adddup2_trampoline_addr,
			uintptr(unsafe.Pointer(&s.os.nullFacts)), nfd, i, 0, 0, 0); e != 0 {
			s.destroyNullFacts()
			return &Error{Name: s.path, Err: os.NewSyscallError("posix_spawn_file_actions_adddup2", e)}
		}
	}
	if s.dirp != nil {
		// addchdir_np copies the path string, so the arena pointer need
		// not outlive this call.
		if e := libcCallRaw(libc_posix_spawn_file_actions_addchdir_np_trampoline_addr,
			uintptr(unsafe.Pointer(&s.os.nullFacts)), uintptr(unsafe.Pointer(s.dirp)), 0, 0, 0, 0); e != 0 {
			s.destroyNullFacts()
			return &Error{Name: s.path, Err: os.NewSyscallError("posix_spawn_file_actions_addchdir_np", e)}
		}
	}
	runtime.AddCleanup(s, func(facts uintptr) {
		libcCallRaw(libc_posix_spawn_file_actions_destroy_trampoline_addr,
			uintptr(unsafe.Pointer(&facts)), 0, 0, 0, 0, 0)
	}, s.os.nullFacts)
	return nil
}

// destroyNullFacts releases a partially built frozen file-actions
// object on the NewRunner error path.
func (s *Runner) destroyNullFacts() {
	libcCallRaw(libc_posix_spawn_file_actions_destroy_trampoline_addr,
		uintptr(unsafe.Pointer(&s.os.nullFacts)), 0, 0, 0, 0, 0)
	s.os.nullFacts = 0
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

// wait blocks until the process exits, storing its final state into ps.
func (p *Process) wait(ps *ProcessState) error {
	ps.pid = p.Pid
	for {
		if _, err := unix.Wait4(p.Pid, &ps.status, 0, &ps.rusage); err == nil {
			break
		} else if err != unix.EINTR {
			return os.NewSyscallError("wait4", err)
		}
	}
	p.sigMu.Lock()
	p.done = true
	p.sigMu.Unlock()
	return nil
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

// reset returns the Process to its pre-start state for Cmd reuse; the
// embedded mutex is retained.
func (p *Process) reset() {
	p.Pid = 0
	p.done = false
}

// release is a no-op on Darwin; there is no handle beyond the PID.
func (p *Process) release() {}
