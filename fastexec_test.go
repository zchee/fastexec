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

package fastexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestRunOutput(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cmd     func() *Cmd
		want    string
		wantErr bool
	}{
		"success: echo single argument": {
			cmd:  func() *Cmd { return Command("echo", "hello") },
			want: "hello\n",
		},
		"success: echo multiple arguments with spaces": {
			cmd:  func() *Cmd { return Command("echo", "a b", "c") },
			want: "a b c\n",
		},
		"success: shell pipeline": {
			cmd:  func() *Cmd { return Command("sh", "-c", "printf foo | tr a-z A-Z") },
			want: "FOO",
		},
		"success: absolute path": {
			cmd:  func() *Cmd { return Command("/bin/echo", "abs") },
			want: "abs\n",
		},
		"error: nonexistent command in PATH": {
			cmd:     func() *Cmd { return Command("fastexec-test-definitely-not-a-command") },
			wantErr: true,
		},
		"error: nonexistent absolute path": {
			cmd:     func() *Cmd { return Command("/nonexistent/fastexec/binary") },
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out, err := tt.cmd().Output()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got output %q", out)
				}
				t.Logf("got expected error: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("Output() failed: %v", err)
			}
			if diff := gocmp.Diff(tt.want, string(out)); diff != "" {
				t.Fatalf("output mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestExecFailureErrno(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		path      string
		wantErrno syscall.Errno
	}{
		"error: ENOENT for missing binary": {
			path:      "/nonexistent/fastexec/binary",
			wantErrno: syscall.ENOENT,
		},
		"error: EACCES for non-executable file": {
			path:      mkNonExecutable(t),
			wantErrno: syscall.EACCES,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := Command(tt.path).Run()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var errno syscall.Errno
			if !errors.As(err, &errno) {
				t.Fatalf("error %v (%T) does not wrap syscall.Errno", err, err)
			}
			if diff := gocmp.Diff(tt.wantErrno, errno); diff != "" {
				t.Fatalf("errno mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// mkNonExecutable creates a regular non-executable file for EACCES tests.
func mkNonExecutable(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExitCode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args     []string
		wantCode int
		wantOK   bool
	}{
		"success: exit 0": {
			args:     []string{"-c", "exit 0"},
			wantCode: 0,
			wantOK:   true,
		},
		"error: exit 1": {
			args:     []string{"-c", "exit 1"},
			wantCode: 1,
		},
		"error: exit 42": {
			args:     []string{"-c", "exit 42"},
			wantCode: 42,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := Command("sh", tt.args...)
			err := c.Run()
			if tt.wantOK {
				if err != nil {
					t.Fatalf("Run() failed: %v", err)
				}
			} else {
				if _, ok := errors.AsType[*ExitError](err); !ok {
					t.Fatalf("error %v (%T) is not *ExitError", err, err)
				}
			}
			if c.ProcessState.Pid() == 0 {
				t.Fatal("ProcessState not populated after Run")
			}
			if got := c.ProcessState.ExitCode(); got != tt.wantCode {
				t.Fatalf("ExitCode() = %d, want %d", got, tt.wantCode)
			}
			if got := c.ProcessState.Success(); got != tt.wantOK {
				t.Fatalf("Success() = %v, want %v", got, tt.wantOK)
			}
		})
	}
}

func TestEnv(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		env  []string
		want string
	}{
		"success: explicit environment": {
			env:  []string{"FASTEXEC_TEST=explicit-value"},
			want: "explicit-value",
		},
		"success: empty value": {
			env:  []string{"FASTEXEC_TEST="},
			want: "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := Command("sh", "-c", "printf '%s' \"$FASTEXEC_TEST\"")
			c.Env = tt.env
			out, err := c.Output()
			if err != nil {
				t.Fatalf("Output() failed: %v", err)
			}
			if diff := gocmp.Diff(tt.want, string(out)); diff != "" {
				t.Fatalf("environment mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInheritedEnv(t *testing.T) {
	t.Setenv("FASTEXEC_INHERIT_TEST", "from-parent")
	// The nil-Env snapshot is cached process-wide; drop any snapshot
	// taken before Setenv and leave a clean slate behind.
	InvalidateEnv()
	t.Cleanup(InvalidateEnv)

	out, err := Command("sh", "-c", "printf '%s' \"$FASTEXEC_INHERIT_TEST\"").Output()
	if err != nil {
		t.Fatalf("Output() failed: %v", err)
	}
	if diff := gocmp.Diff("from-parent", string(out)); diff != "" {
		t.Fatalf("inherited environment mismatch (-want +got):\n%s", diff)
	}
}

func TestDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	c := Command("pwd")
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		t.Fatalf("Output() failed: %v", err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSuffix(string(out), "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(resolved, got); diff != "" {
		t.Fatalf("working directory mismatch (-want +got):\n%s", diff)
	}
}

func TestStdin(t *testing.T) {
	t.Parallel()

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	const payload = "stdin-payload\n"
	go func() {
		pw.WriteString(payload)
		pw.Close()
	}()

	c := Command("cat")
	c.Stdin = pr
	out, err := c.Output()
	pr.Close()
	if err != nil {
		t.Fatalf("Output() failed: %v", err)
	}
	if diff := gocmp.Diff(payload, string(out)); diff != "" {
		t.Fatalf("stdin round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestCombinedOutput(t *testing.T) {
	t.Parallel()

	out, err := Command("sh", "-c", "printf out; printf err 1>&2").CombinedOutput()
	if err != nil {
		t.Fatalf("CombinedOutput() failed: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Fatalf("combined output %q missing streams", got)
	}
}

func TestOutputCapturesStderrInExitError(t *testing.T) {
	t.Parallel()

	_, err := Command("sh", "-c", "printf boom 1>&2; exit 3").Output()
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error %v (%T) is not *ExitError", err, err)
	}
	if diff := gocmp.Diff("boom", string(ee.Stderr)); diff != "" {
		t.Fatalf("captured stderr mismatch (-want +got):\n%s", diff)
	}
	if got := ee.ExitCode(); got != 3 {
		t.Fatalf("ExitCode() = %d, want 3", got)
	}
}

func TestContextCancelKillsProcess(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	c := CommandContext(ctx, "sleep", "30")
	if err := c.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	start := time.Now()
	time.AfterFunc(50*time.Millisecond, cancel)
	err := c.Wait()
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() = %v, want context.Canceled", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("process was not killed promptly, Wait took %v", elapsed)
	}
	if c.ProcessState.Pid() == 0 {
		t.Fatal("ProcessState not populated after Wait")
	}
	if got := c.ProcessState.Sys().Signal(); got != syscall.SIGKILL {
		t.Fatalf("termination signal = %v, want SIGKILL", got)
	}
}

func TestContextAlreadyCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := CommandContext(ctx, "echo").Run()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
}

func TestSignal(t *testing.T) {
	t.Parallel()

	c := Command("sleep", "30")
	if err := c.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	if err := c.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal() failed: %v", err)
	}
	err := c.Wait()
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Wait() = %v (%T), want *ExitError", err, err)
	}
	if got := ee.Sys().Signal(); got != syscall.SIGTERM {
		t.Fatalf("termination signal = %v, want SIGTERM", got)
	}
	if err := c.Process.Signal(syscall.SIGTERM); !errors.Is(err, ErrProcessDone) {
		t.Fatalf("Signal() after Wait = %v, want ErrProcessDone", err)
	}
}

func TestConcurrentSpawn(t *testing.T) {
	t.Parallel()

	const workers = 32
	var wg sync.WaitGroup
	errc := make(chan error, workers)
	for i := range workers {
		wg.Go(func() {
			c := Command("sh", "-c", "exit 0")
			c.Env = []string{"FASTEXEC_WORKER=" + itoa(i)}
			if err := c.Run(); err != nil {
				errc <- err
			}
		})
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Errorf("concurrent Run() failed: %v", err)
	}
}

func TestArgvZero(t *testing.T) {
	t.Parallel()

	c := Command("sh", "-c", "printf '%s' \"$0\"")
	c.Args = []string{"custom-argv0", "-c", "printf '%s' \"$0\""}
	out, err := c.Output()
	if err != nil {
		t.Fatalf("Output() failed: %v", err)
	}
	if diff := gocmp.Diff("custom-argv0", string(out)); diff != "" {
		t.Fatalf("argv[0] mismatch (-want +got):\n%s", diff)
	}
}

func TestReset(t *testing.T) {
	t.Parallel()

	c := Command("sh", "-c", "exit 7")
	c.Env = []string{}
	err := c.Run()
	var first *ExitError
	if !errors.As(err, &first) {
		t.Fatalf("first Run() = %v (%T), want *ExitError", err, err)
	}

	// The same Cmd respawns after Reset, and the prior run's ExitError
	// keeps its own copy of the process state.
	c.Reset()
	if c.ProcessState.Pid() != 0 {
		t.Fatal("Reset did not clear ProcessState")
	}
	err = c.Run()
	var second *ExitError
	if !errors.As(err, &second) {
		t.Fatalf("second Run() = %v (%T), want *ExitError", err, err)
	}
	if first.Pid() == second.Pid() {
		t.Fatalf("both runs report pid %d, want distinct processes", first.Pid())
	}
	if got := first.ExitCode(); got != 7 {
		t.Fatalf("first ExitError.ExitCode() after Reset = %d, want 7", got)
	}

	// The state machine still rejects out-of-order calls after Reset.
	c.Reset()
	if err := c.Wait(); err == nil {
		t.Fatal("Wait() before Start() should fail after Reset")
	}
}

func TestNulByteRejected(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cmd func() *Cmd
	}{
		"error: NUL in argument": {
			cmd: func() *Cmd { return Command("echo", "a\x00b") },
		},
		"error: NUL in environment": {
			cmd: func() *Cmd {
				c := Command("echo")
				c.Env = []string{"A=\x00"}
				return c
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := tt.cmd().Run()
			if !errors.Is(err, syscall.EINVAL) {
				t.Fatalf("Run() = %v, want EINVAL", err)
			}
		})
	}
}

func TestStateErrors(t *testing.T) {
	t.Parallel()

	c := Command("echo")
	if err := c.Wait(); err == nil {
		t.Fatal("Wait() before Start() should fail")
	}

	c2 := Command("echo")
	if err := c2.Run(); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if err := c2.Start(); err == nil {
		t.Fatal("second Start() should fail")
	}
	if err := c2.Wait(); err == nil {
		t.Fatal("second Wait() should fail")
	}
}

func TestNoFDLeakIntoChild(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "darwin" {
		// On Linux non-CLOEXEC descriptors are inherited, matching
		// os/exec; only Darwin's POSIX_SPAWN_CLOEXEC_DEFAULT closes
		// every descriptor that was not explicitly mapped.
		t.Skip("descriptor-closing guarantee is Darwin-only")
	}

	// Create a descriptor with O_CLOEXEC deliberately cleared via dup(2);
	// it must still not appear in the child.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	leaked, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(leaked)

	err = Command("sh", "-c", "test ! -e /dev/fd/"+itoa(leaked)).Run()
	if err != nil {
		t.Fatalf("child unexpectedly inherited fd %d: %v", leaked, err)
	}
}

func TestProcessStateString(t *testing.T) {
	t.Parallel()

	c := Command("sh", "-c", "exit 7")
	err := c.Run()
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error %v (%T) is not *ExitError", err, err)
	}
	if diff := gocmp.Diff("exit status 7", ee.Error()); diff != "" {
		t.Fatalf("ExitError.Error() mismatch (-want +got):\n%s", diff)
	}
}

func TestLargeOutput(t *testing.T) {
	t.Parallel()

	// 1 MiB of output exercises pipe draining beyond the pipe buffer.
	out, err := Command("sh", "-c", "head -c 1048576 /dev/zero").Output()
	if err != nil {
		t.Fatalf("Output() failed: %v", err)
	}
	if len(out) != 1048576 {
		t.Fatalf("got %d bytes, want 1048576", len(out))
	}
}
