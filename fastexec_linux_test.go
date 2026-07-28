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
	"errors"
	"os"
	"runtime/pprof"
	"sync"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestClearSighandFallbackLatch forces the clearSighandUnavailable latch
// and proves the signal-mask fallback spawn path (kernel < 5.5, or the
// clone(2) seccomp fallback) still spawns, reports exec errors, and
// captures output. Deliberately not parallel: the latch is
// process-global, and this test runs in the sequential phase before any
// paused parallel test resumes.
func TestClearSighandFallbackLatch(t *testing.T) {
	old := clearSighandUnavailable.Load()
	clearSighandUnavailable.Store(true)
	t.Cleanup(func() { clearSighandUnavailable.Store(old) })

	out, err := Command("sh", "-c", "printf latched").Output()
	if err != nil {
		t.Fatalf("Output() on latched mask path failed: %v", err)
	}
	if diff := gocmp.Diff("latched", string(out)); diff != "" {
		t.Fatalf("output mismatch (-want +got):\n%s", diff)
	}

	err = Command("/nonexistent/fastexec/binary").Run()
	var errno syscall.Errno
	if !errors.As(err, &errno) || errno != syscall.ENOENT {
		t.Fatalf("exec failure on latched mask path = %v, want ENOENT", err)
	}
}

// TestSpawnUnderCPUProfile spawns a burst of children while the CPU
// profiler delivers SIGPROF to the parent, on both the
// CLONE_CLEAR_SIGHAND fast path and the forced signal-mask fallback. A
// SIGPROF landing in a pre-execve child would terminate it (SIG_DFL is
// "profiling timer expired"), so every child exiting 0 is the
// assertion. Not parallel: it toggles the process-global latch and the
// profiler.
func TestSpawnUnderCPUProfile(t *testing.T) {
	f, err := os.Create(t.TempDir() + "/cpu.pprof")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatal(err)
	}
	defer pprof.StopCPUProfile()

	spawns := 1024
	if testing.Short() {
		spawns = 256
	}

	for _, tt := range []struct {
		name  string
		latch bool
	}{
		{name: "success: clear-sighand fast path", latch: false},
		{name: "success: forced signal-mask fallback", latch: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old := clearSighandUnavailable.Load()
			clearSighandUnavailable.Store(tt.latch)
			t.Cleanup(func() { clearSighandUnavailable.Store(old) })

			s, err := NewSpec("/bin/true", nil, []string{}, "")
			if err != nil {
				s, err = NewSpec("/usr/bin/true", nil, []string{}, "")
			}
			if err != nil {
				t.Skipf("no `true` binary: %v", err)
			}
			const workers = 16
			var wg sync.WaitGroup
			errc := make(chan error, workers)
			for range workers {
				wg.Go(func() {
					for range spawns / workers {
						var ps ProcessState
						if err := s.Run(nil, nil, nil, &ps); err != nil {
							errc <- err
							return
						}
					}
				})
			}
			wg.Wait()
			close(errc)
			for err := range errc {
				t.Fatalf("spawn under CPU profiling failed: %v", err)
			}
		})
	}
}
