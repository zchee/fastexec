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
