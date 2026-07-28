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
	"runtime/debug"
	"sync"
	"testing"
)

// TestPooledAttrGCSoak hammers the spawn path while the GC runs almost
// continuously, cycling spawnState values through sync.Pool eviction so
// that the runtime.AddCleanup-driven posix_spawnattr_t destruction
// races live spawns. A premature destroy or double free corrupts the C
// heap and crashes libSystem, so surviving the soak is the assertion.
func TestPooledAttrGCSoak(t *testing.T) {
	old := debug.SetGCPercent(1)
	t.Cleanup(func() { debug.SetGCPercent(old) })

	spawns := 4096
	if testing.Short() {
		spawns = 512
	}
	const workers = 16
	var wg sync.WaitGroup
	errc := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			for range spawns / workers {
				c := Command("/usr/bin/true")
				c.Env = []string{}
				if err := c.Run(); err != nil {
					errc <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatalf("spawn under GOGC=1 failed: %v", err)
	}
}
