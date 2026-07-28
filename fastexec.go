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

//go:build (linux && (amd64 || arm64)) || darwin

// Package fastexec is a high-throughput, low-latency alternative to [os/exec]
// for spawning external processes on Linux and Darwin (macOS).
//
// The standard [os/exec] package favors portability and conservative safety:
// every spawn serializes on the global [syscall.ForkLock], converts argv and
// environment strings with per-string heap allocations, and (on cancel)
// signals the child by PID, which is subject to PID-reuse races. fastexec
// removes those costs with platform-specific process-creation primitives:
//
//   - Linux: processes are created with a raw clone3(2) system call using
//     CLONE_VFORK|CLONE_VM, so the parent's page tables are never copied
//     regardless of the parent's heap size, and CLONE_PIDFD, so cancellation
//     and signaling use pidfd_send_signal(2) and can never hit a recycled
//     PID. The child runs entirely in hand-written assembly on a private
//     stack until execve(2), and syscall.ForkLock is not taken: file
//     descriptors are managed exclusively with atomic O_CLOEXEC operations
//     (F_DUPFD_CLOEXEC), so spawn throughput scales linearly with CPU
//     count. Requires Linux 5.4+ (clone3 and waitid P_PIDFD).
//
//   - Darwin: processes are created with posix_spawn(2), the only
//     fork-safe primitive Apple supports for multithreaded programs; the
//     kernel handles the intermediate child state, so pthread_atfork
//     handler deadlocks and copy-on-write stalls are avoided entirely.
//     posix_spawn is reached through a direct libSystem call (the same
//     mechanism the Go runtime itself uses), not cgo, so there is no cgo
//     call overhead. POSIX_SPAWN_CLOEXEC_DEFAULT guarantees that no file
//     descriptor other than the requested stdio descriptors leaks into the
//     child, again without [syscall.ForkLock].
//
// On both platforms argv, envp, and auxiliary strings are marshaled into a
// pooled, reusable arena as NUL-terminated C strings referenced by raw
// pointers, so steady-state spawning performs no per-string heap
// allocations.
//
// # Differences from os/exec
//
// The API mirrors os/exec where practical, with deliberate deviations that
// keep the hot path allocation- and goroutine-free:
//
//   - Stdin, Stdout, and Stderr are [*os.File], not [io.Reader]/[io.Writer]. A
//     nil field means /dev/null (a shared descriptor, opened once). Use
//     [os.Pipe] for streaming; [Cmd.Output] and [Cmd.CombinedOutput] do this
//     internally.
//   - Environment entries are passed to the kernel as-is: duplicates are
//     not deduplicated. If Env is nil the current environment is used
//     (which allocates); reuse a cached Env slice for zero-allocation
//     spawning.
//   - The child always starts with all signal dispositions reset to their
//     defaults and an empty signal mask.
//
// A Cmd cannot be reused after calling [Cmd.Start], [Cmd.Run],
// [Cmd.Output], or [Cmd.CombinedOutput], and its methods must not be
// called concurrently, matching [os/exec].
package fastexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ErrProcessDone reports that a [Process] has already finished and been
// reaped.
var ErrProcessDone = errors.New("fastexec: process already finished")

// Error is returned by [Cmd.Start] and [LookPath]-time failures when a
// command cannot be started.
type Error struct {
	// Name is the file name for which the error occurred.
	Name string
	// Err is the underlying error.
	Err error
}

// Error implements the error interface.
func (e *Error) Error() string {
	return "fastexec: " + e.Name + ": " + e.Err.Error()
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// ExitError reports an unsuccessful exit by a command. It embeds its
// own copy of the final [ProcessState], so it remains valid after the
// originating [Cmd] is reset or reused.
type ExitError struct {
	ProcessState

	// Stderr holds the standard error output of the command if it was
	// captured by [Cmd.Output] and the command's Stderr was nil.
	Stderr []byte
}

// Error implements the error interface.
func (e *ExitError) Error() string { return e.ProcessState.String() }

// ProcessState stores information about a process, as reported by Wait.
type ProcessState struct {
	pid    int
	status unix.WaitStatus
	rusage unix.Rusage
}

// Pid returns the process id of the exited process.
func (p *ProcessState) Pid() int { return p.pid }

// Exited reports whether the program has exited (as opposed to being
// terminated by a signal).
func (p *ProcessState) Exited() bool { return p.status.Exited() }

// Success reports whether the program exited successfully, such as with
// exit status 0 on Unix.
func (p *ProcessState) Success() bool {
	return p.status.Exited() && p.status.ExitStatus() == 0
}

// ExitCode returns the exit code of the exited process, or -1 if the
// process was terminated by a signal.
func (p *ProcessState) ExitCode() int {
	if !p.status.Exited() {
		return -1
	}
	return p.status.ExitStatus()
}

// Sys returns the system-dependent exit information about the process as a
// [syscall.WaitStatus].
func (p *ProcessState) Sys() unix.WaitStatus { return p.status }

// SysUsage returns the system-dependent resource usage information about
// the exited process.
func (p *ProcessState) SysUsage() *unix.Rusage { return &p.rusage }

// String returns a human-readable description of the exit state.
func (p *ProcessState) String() string {
	switch {
	case p.status.Exited():
		return "exit status " + itoa(p.status.ExitStatus())
	case p.status.Signaled():
		s := "signal: " + p.status.Signal().String()
		if p.status.CoreDump() {
			s += " (core dumped)"
		}
		return s
	case p.status.Stopped():
		return "stop signal: " + p.status.StopSignal().String()
	default:
		return "wait status: " + itoa(int(p.status))
	}
}

// itoa formats v in decimal without pulling in strconv's full formatting
// paths on the error path.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Cmd represents an external command being prepared or run.
//
// After Run, Start, Output, or CombinedOutput has been called, a Cmd
// must be returned to its pre-start state with [Cmd.Reset] before it
// can spawn again. A Cmd must not be copied after first use: it
// embeds the process handle and its synchronization state by value.
type Cmd struct {
	// Path is the path of the command to run.
	//
	// This is the only field that must be set to a non-zero value. If
	// Path is relative, it is evaluated relative to Dir.
	Path string

	// Args holds command line arguments, including the command as
	// Args[0]. If Args is empty, Path is used as the sole argument.
	Args []string

	// Env specifies the environment of the process. Each entry is of the
	// form "key=value". If Env is nil, the new process uses the current
	// process's environment (obtained via os.Environ, which allocates).
	// Entries are passed to the kernel verbatim; duplicates are not
	// deduplicated.
	Env []string

	// Dir specifies the working directory of the command. If Dir is
	// empty, the command runs in the calling process's current directory.
	Dir string

	// Stdin, Stdout, and Stderr specify the process's standard input,
	// output, and error. A nil field means the corresponding descriptor
	// is connected to /dev/null.
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File

	// Process is the underlying process, valid once Start has
	// succeeded. It is embedded by value so steady-state spawning
	// allocates nothing; take its address to call its methods
	// concurrently.
	Process Process

	// ProcessState contains information about the exited process,
	// populated by Wait (its Pid method reports zero until then). It is
	// embedded by value; [ExitError] carries its own copy.
	ProcessState ProcessState

	ctx         context.Context
	lookPathErr error
	started     bool
	finished    bool
	waitDone    chan struct{}
	watcherDone chan struct{}
	ctxKill     atomic.Bool
}

// Command returns a [Cmd] to execute the named program with the given
// arguments, following the same PATH-resolution rules as os/exec.Command.
func Command(name string, arg ...string) *Cmd {
	c := &Cmd{
		Path: name,
		Args: append([]string{name}, arg...),
	}
	if filepath.Base(name) == name {
		lp, err := exec.LookPath(name)
		if lp != "" {
			// Update the path even on ErrDot, matching os/exec.
			c.Path = lp
		}
		if err != nil {
			c.lookPathErr = err
		}
	}
	return c
}

// CommandContext is like [Command] but includes a context.
//
// The provided context is used to kill the process (with SIGKILL) if the
// context becomes done before the command completes on its own. On Linux
// the kill is delivered through the process's pidfd and can never affect a
// recycled PID.
func CommandContext(ctx context.Context, name string, arg ...string) *Cmd {
	if ctx == nil {
		panic("fastexec: nil Context")
	}
	c := Command(name, arg...)
	c.ctx = ctx
	return c
}

// String returns a human-readable description of c. It is intended only
// for debugging.
func (c *Cmd) String() string {
	if c.lookPathErr != nil {
		return c.Path
	}
	var b strings.Builder
	b.WriteString(c.Path)
	for _, a := range c.Args[1:] {
		b.WriteByte(' ')
		b.WriteString(a)
	}
	return b.String()
}

// argv returns the argument vector, defaulting to Path when Args is empty.
func (c *Cmd) argv() []string {
	if len(c.Args) > 0 {
		return c.Args
	}
	return []string{c.Path}
}

// devNull is the shared /dev/null descriptor substituted for nil stdio
// fields. It is opened once and intentionally never closed.
var devNull = sync.OnceValues(func() (*os.File, error) {
	return os.OpenFile(os.DevNull, os.O_RDWR, 0)
})

// prepStdio hands stdio sources already at fd >= 3 (the /dev/null
// singleton, os.Pipe ends, any normal file) to the child as-is: with
// every source above the target range 0..2, the child-side dup2/dup3
// wiring cannot collide by construction. Only sources at fds 0-2 (a
// caller passing os.Stdin et al) are duplicated up to >= 3 with
// O_CLOEXEC set atomically; closeDupped releases those temporaries.
// Non-CLOEXEC sources appear in the child at their original number on
// Linux, exactly as with os/exec, while Darwin's CLOEXEC_DEFAULT still
// closes everything that was not explicitly mapped.
func prepStdio(files [3]*os.File) (highs [3]int, dupped [3]bool, err error) {
	for i, f := range files {
		fd := f.Fd()
		if fd >= 3 {
			highs[i] = int(fd)
			continue
		}
		h, ferr := unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 3)
		if ferr != nil {
			closeDupped(highs, dupped)
			return highs, dupped, os.NewSyscallError("fcntl", ferr)
		}
		highs[i] = h
		dupped[i] = true
	}
	return highs, dupped, nil
}

// closeDupped closes the descriptors prepStdio duplicated up.
func closeDupped(highs [3]int, dupped [3]bool) {
	for i, d := range highs {
		if dupped[i] {
			unix.Close(d)
		}
	}
}

// envCache holds the snapshot of the process environment used for
// commands whose Env is nil, so steady-state nil-Env spawning does not
// re-copy the environment on every spawn.
var envCache atomic.Pointer[[]string]

// defaultEnv returns the cached copy of the current environment,
// snapshotting os.Environ on first use.
func defaultEnv() []string {
	if p := envCache.Load(); p != nil {
		return *p
	}
	env := os.Environ()
	envCache.Store(&env)
	return env
}

// InvalidateEnv discards the environment snapshot used for commands
// with a nil Env. A caller that mutates the process environment
// (os.Setenv, os.Unsetenv, os.Clearenv) after a spawn has occurred
// must call InvalidateEnv for later nil-Env commands to observe the
// change; explicit Env slices are unaffected.
func InvalidateEnv() {
	envCache.Store(nil)
}

// Start starts the specified command but does not wait for it to complete.
//
// After a successful call to Start the [Cmd.Wait] method must be called in
// order to release associated system resources.
func (c *Cmd) Start() error {
	if c.started {
		return errors.New("fastexec: already started")
	}
	c.started = true
	if c.lookPathErr != nil {
		return &Error{Name: c.Path, Err: c.lookPathErr}
	}
	if c.Path == "" {
		return &Error{Name: c.Path, Err: errors.New("no command")}
	}
	if c.ctx != nil && c.ctx.Err() != nil {
		return c.ctx.Err()
	}

	files := [3]*os.File{c.Stdin, c.Stdout, c.Stderr}
	for i, f := range files {
		if f == nil {
			null, err := devNull()
			if err != nil {
				return &Error{Name: c.Path, Err: err}
			}
			files[i] = null
		}
	}

	if err := startProcess(c, files); err != nil {
		return err
	}

	if c.ctx != nil && c.ctx.Done() != nil {
		c.waitDone = make(chan struct{})
		c.watcherDone = make(chan struct{})
		go func() {
			defer close(c.watcherDone)
			select {
			case <-c.ctx.Done():
				c.ctxKill.Store(true)
				_ = c.Process.Kill()
			case <-c.waitDone:
			}
		}()
	}
	return nil
}

// Wait waits for the command to exit. It must have been started by
// [Cmd.Start].
//
// If the command runs and exits with a non-zero status, the error is of
// type [*ExitError]. If the command was killed because its context became
// done, the context's error is returned instead.
func (c *Cmd) Wait() error {
	if !c.started {
		return errors.New("fastexec: not started")
	}
	if c.finished {
		return errors.New("fastexec: Wait was already called")
	}
	c.finished = true

	err := c.Process.wait(&c.ProcessState)
	if c.waitDone != nil {
		close(c.waitDone)
		<-c.watcherDone
	}
	c.Process.release()
	if err != nil {
		return err
	}

	if c.ctxKill.Load() {
		if ctxErr := c.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if !c.ProcessState.Success() {
		return &ExitError{ProcessState: c.ProcessState}
	}
	return nil
}

// Reset returns c to its pre-start state so the same Cmd can spawn
// again, keeping the command configuration (Path, Args, Env, Dir,
// stdio, context) and the marshaling state. Together with a preset Env
// this makes repeated Run loops allocation-free.
//
// Reset must not be called while a process started from c is still
// running. An [ExitError] returned by a previous run carries its own
// copy of the process state and stays valid across Reset.
func (c *Cmd) Reset() {
	if c.started && !c.finished && c.Process.Pid != 0 {
		panic("fastexec: Reset called on a running Cmd")
	}
	c.started = false
	c.finished = false
	c.waitDone = nil
	c.watcherDone = nil
	c.ctxKill.Store(false)
	c.Process.reset()
	c.ProcessState = ProcessState{}
}

// Wait blocks until the process exits, stores its final state into ps,
// and releases the process handle. It returns an [*ExitError] if the
// process exited unsuccessfully. Wait must be called exactly once per
// started process; it is used with [Spec.Start], while [Cmd.Wait]
// wraps it with the Cmd bookkeeping.
func (p *Process) Wait(ps *ProcessState) error {
	err := p.wait(ps)
	p.release()
	if err != nil {
		return err
	}
	if !ps.Success() {
		return &ExitError{ProcessState: *ps}
	}
	return nil
}

// Run starts the specified command and waits for it to complete.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// Output runs the command and returns its standard output. If the command
// exits with a non-zero status and c.Stderr was nil, the returned
// [*ExitError] retains up to 32 KiB of the standard error output in its
// Stderr field.
func (c *Cmd) Output() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("fastexec: Stdout already set")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c.Stdout = pw

	var (
		errPipe   *os.File
		errBuf    bytes.Buffer
		errDrain  chan struct{}
		errLimitW io.Writer
	)
	if c.Stderr == nil {
		var epr, epw *os.File
		epr, epw, err = os.Pipe()
		if err != nil {
			pr.Close()
			pw.Close()
			return nil, err
		}
		c.Stderr = epw
		errPipe = epr
		errDrain = make(chan struct{})
		errLimitW = &limitedWriter{w: &errBuf, n: 32 << 10}
		go func() {
			defer close(errDrain)
			io.Copy(errLimitW, epr) //nolint:errcheck // best-effort capture
		}()
	}

	startErr := c.Start()
	pw.Close()
	if c.Stderr != nil && errPipe != nil {
		c.Stderr.Close()
	}
	if startErr != nil {
		pr.Close()
		if errPipe != nil {
			errPipe.Close()
			<-errDrain
		}
		return nil, startErr
	}

	var out bytes.Buffer
	_, readErr := out.ReadFrom(pr)
	pr.Close()
	if errPipe != nil {
		<-errDrain
		errPipe.Close()
	}

	waitErr := c.Wait()
	if waitErr != nil {
		if ee, ok := errors.AsType[*ExitError](waitErr); ok {
			ee.Stderr = errBuf.Bytes()
		}
		return out.Bytes(), waitErr
	}
	if readErr != nil {
		return out.Bytes(), readErr
	}
	return out.Bytes(), nil
}

// CombinedOutput runs the command and returns its combined standard output
// and standard error.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("fastexec: Stdout already set")
	}
	if c.Stderr != nil {
		return nil, errors.New("fastexec: Stderr already set")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c.Stdout = pw
	c.Stderr = pw

	startErr := c.Start()
	pw.Close()
	if startErr != nil {
		pr.Close()
		return nil, startErr
	}

	var out bytes.Buffer
	_, readErr := out.ReadFrom(pr)
	pr.Close()

	if waitErr := c.Wait(); waitErr != nil {
		return out.Bytes(), waitErr
	}
	if readErr != nil {
		return out.Bytes(), readErr
	}
	return out.Bytes(), nil
}

// limitedWriter writes to w until n bytes have been written, then
// discards further input while still reporting success so the source pipe
// keeps draining.
type limitedWriter struct {
	w io.Writer
	n int
}

// Write implements io.Writer.
func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	keep := min(len(p), l.n)
	if _, err := l.w.Write(p[:keep]); err != nil {
		return 0, err
	}
	l.n -= keep
	return len(p), nil
}

// cstrs is a reusable arena that marshals Go strings into NUL-terminated C
// strings and pointer vectors without per-string heap allocations.
type cstrs struct {
	buf  []byte
	ptrs []*byte
}

// build lays out path, dir (optional), argv, and env in the arena and
// returns raw pointers suitable for execve(2)/posix_spawn(2). The returned
// pointers are valid until the next call to build; the caller must keep
// the cstrs value reachable while the pointers are in use.
func (cs *cstrs) build(path, dir string, argv, env []string) (pathp, dirp *byte, argvp, envp **byte, err error) {
	need := len(path) + 1
	if dir != "" {
		need += len(dir) + 1
	}
	for _, s := range argv {
		need += len(s) + 1
	}
	for _, s := range env {
		need += len(s) + 1
	}
	if cap(cs.buf) < need {
		cs.buf = make([]byte, 0, need+64)
	} else {
		cs.buf = cs.buf[:0]
	}
	nptr := len(argv) + len(env) + 2
	if cap(cs.ptrs) < nptr {
		cs.ptrs = make([]*byte, 0, nptr)
	} else {
		cs.ptrs = cs.ptrs[:0]
	}

	// The buffer never reallocates below because the full capacity was
	// reserved above, so pointers taken during the fill remain valid.
	put := func(s string) (*byte, error) {
		if strings.IndexByte(s, 0) != -1 {
			return nil, unix.EINVAL
		}
		off := len(cs.buf)
		cs.buf = append(cs.buf, s...)
		cs.buf = append(cs.buf, 0)
		return &cs.buf[off], nil
	}

	if pathp, err = put(path); err != nil {
		return nil, nil, nil, nil, err
	}
	if dir != "" {
		if dirp, err = put(dir); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	for _, s := range argv {
		p, perr := put(s)
		if perr != nil {
			return nil, nil, nil, nil, perr
		}
		cs.ptrs = append(cs.ptrs, p)
	}
	cs.ptrs = append(cs.ptrs, nil)
	envIdx := len(cs.ptrs)
	for _, s := range env {
		p, perr := put(s)
		if perr != nil {
			return nil, nil, nil, nil, perr
		}
		cs.ptrs = append(cs.ptrs, p)
	}
	cs.ptrs = append(cs.ptrs, nil)

	argvp = (**byte)(unsafe.Pointer(&cs.ptrs[0]))
	envp = (**byte)(unsafe.Pointer(&cs.ptrs[envIdx]))
	return pathp, dirp, argvp, envp, nil
}
