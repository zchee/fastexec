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
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// testTrue returns a minimal no-op binary path.
func testTrue(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/usr/bin/true", "/bin/true"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no `true` binary found")
	return ""
}

// TestAllocsRunnerRun asserts the acceptance criterion that a warmed
// Runner.Run performs zero heap allocations per spawn.
func TestAllocsRunnerRun(t *testing.T) {
	if raceEnabled {
		t.Skip("race instrumentation perturbs allocation counts")
	}
	s, err := NewRunner(testTrue(t), nil, []string{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var ps ProcessState
	// Warm up: latch the runtime probes and populate the spawn pool.
	if err := s.Run(nil, nil, nil, &ps); err != nil {
		t.Fatal(err)
	}
	n := testing.AllocsPerRun(100, func() {
		if err := s.Run(nil, nil, nil, &ps); err != nil {
			t.Fatal(err)
		}
	})
	if n != 0 {
		t.Fatalf("Runner.Run allocates %v/op, want 0", n)
	}
}

// TestAllocsCmdRunReset asserts the acceptance criterion that a Cmd
// with a preset Env respawned through Reset performs zero heap
// allocations per spawn.
func TestAllocsCmdRunReset(t *testing.T) {
	if raceEnabled {
		t.Skip("race instrumentation perturbs allocation counts")
	}
	c := Command(testTrue(t))
	c.Env = []string{}
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	c.Reset()
	n := testing.AllocsPerRun(100, func() {
		if err := c.Run(); err != nil {
			t.Fatal(err)
		}
		c.Reset()
	})
	if n != 0 {
		t.Fatalf("Cmd Run+Reset allocates %v/op, want 0", n)
	}
}

// TestStdioFdMatrix exercises the direct-fd fast path and the dup-up
// fallback across source-descriptor placements: sources below 3 (the
// parent's own stdio), sources at >= 3 (regular files), and the same
// source duplicated onto both stdout and stderr.
func TestStdioFdMatrix(t *testing.T) {
	t.Parallel()

	t.Run("success: parent stdio at fds 0-2 forces dup-up", func(t *testing.T) {
		t.Parallel()
		c := Command("sh", "-c", "exit 0")
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			t.Fatalf("Run() with fds 0-2 failed: %v", err)
		}
	})

	t.Run("success: regular file at fd >= 3 taken directly", func(t *testing.T) {
		t.Parallel()
		f, err := os.CreateTemp(t.TempDir(), "stdout")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		c := Command("sh", "-c", "printf file-out")
		c.Stdout = f
		if err := c.Run(); err != nil {
			t.Fatalf("Run() failed: %v", err)
		}
		got, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "file-out" {
			t.Fatalf("file content = %q, want %q", got, "file-out")
		}
	})

	t.Run("success: one pipe end as both stdout and stderr", func(t *testing.T) {
		t.Parallel()
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer pr.Close()
		c := Command("sh", "-c", "printf out; printf err 1>&2")
		c.Stdout = pw
		c.Stderr = pw
		if err := c.Start(); err != nil {
			pw.Close()
			t.Fatalf("Start() failed: %v", err)
		}
		pw.Close()
		buf := make([]byte, 64)
		n, _ := pr.Read(buf)
		if err := c.Wait(); err != nil {
			t.Fatalf("Wait() failed: %v", err)
		}
		got := string(buf[:n])
		if got != "outerr" && got != "errout" {
			t.Fatalf("combined stream = %q, want out+err", got)
		}
	})
}

// TestNoFDLeakAcrossSpawns runs a spawn-heavy mix (plain runs, pipe
// captures, exec failures, Runner runs) and asserts the parent's
// descriptor table returns to its starting size. Deliberately not
// parallel: it runs in the sequential phase, when no other test can
// hold descriptors open.
func TestNoFDLeakAcrossSpawns(t *testing.T) {
	// Probe descriptors directly with fcntl(F_GETFD); reading /dev/fd
	// races with its own directory fd on Darwin.
	countFDs := func() int {
		n := 0
		for fd := range uintptr(256) {
			if _, err := unix.FcntlInt(fd, unix.F_GETFD, 0); err == nil {
				n++
			}
		}
		return n
	}

	// One of everything first, so singletons (devNull, probe latches)
	// and pools are initialized before the baseline snapshot.
	if err := Command("sh", "-c", "exit 0").Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := Command("sh", "-c", "printf warm").Output(); err != nil {
		t.Fatal(err)
	}
	before := countFDs()

	s, err := NewRunner("sh", []string{"-c", "exit 0"}, []string{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for range 25 {
		if err := Command("sh", "-c", "exit 0").Run(); err != nil {
			t.Fatal(err)
		}
		if _, err := Command("sh", "-c", "printf x").Output(); err != nil {
			t.Fatal(err)
		}
		if err := Command("/nonexistent/fastexec/binary").Run(); err == nil {
			t.Fatal("exec of nonexistent binary succeeded")
		}
		var ps ProcessState
		if err := s.Run(nil, nil, nil, &ps); err != nil {
			t.Fatal(err)
		}
	}

	if after := countFDs(); after != before {
		t.Fatalf("descriptor count changed across spawns: %d -> %d", before, after)
	}
}

// TestNoZombiesAfterSpawns asserts that every child spawned and waited
// above was actually reaped: with no zombies, a WNOHANG wait for any
// child reports nothing reapable. Not parallel: parallel-phase tests
// legitimately have live children.
func TestNoZombiesAfterSpawns(t *testing.T) {
	for range 64 {
		if err := Command("sh", "-c", "exit 0").Run(); err != nil {
			t.Fatal(err)
		}
	}
	var ws syscall.WaitStatus
	pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
	if pid > 0 {
		t.Fatalf("Wait4 reaped unexpected child %d (%v): zombie leak", pid, ws)
	}
	_ = err // ECHILD (no children at all) and pid==0 (none reapable) both pass
}
