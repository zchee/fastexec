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
	"os/exec"
	"testing"
)

// benchTarget resolves a minimal no-op binary once for all benchmarks.
func benchTarget(b *testing.B) string {
	b.Helper()
	for _, p := range []string{"/usr/bin/true", "/bin/true"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	b.Skip("no `true` binary found")
	return ""
}

// benchEnv is a fixed environment reused across iterations so the
// fastexec path stays allocation-free for argument marshaling.
var benchEnv = []string{"PATH=/usr/bin:/bin", "FASTEXEC_BENCH=1"}

func BenchmarkRunFastexec(b *testing.B) {
	path := benchTarget(b)
	b.ReportAllocs()
	for b.Loop() {
		c := Command(path)
		c.Env = benchEnv
		if err := c.Run(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRunFastexecReset(b *testing.B) {
	path := benchTarget(b)
	c := Command(path)
	c.Env = benchEnv
	b.ReportAllocs()
	for b.Loop() {
		if err := c.Run(); err != nil {
			b.Fatal(err)
		}
		c.Reset()
	}
}

func BenchmarkRunSpec(b *testing.B) {
	path := benchTarget(b)
	s, err := NewSpec(path, nil, benchEnv, "")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		var ps ProcessState
		if err := s.Run(nil, nil, nil, &ps); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRunParallelSpec(b *testing.B) {
	path := benchTarget(b)
	s, err := NewSpec(path, nil, benchEnv, "")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var ps ProcessState
			if err := s.Run(nil, nil, nil, &ps); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkRunOsExec(b *testing.B) {
	path := benchTarget(b)
	b.ReportAllocs()
	for b.Loop() {
		c := exec.Command(path)
		c.Env = benchEnv
		if err := c.Run(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRunParallelFastexec(b *testing.B) {
	path := benchTarget(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c := Command(path)
			c.Env = benchEnv
			if err := c.Run(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkRunParallelOsExec(b *testing.B) {
	path := benchTarget(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c := exec.Command(path)
			c.Env = benchEnv
			if err := c.Run(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
