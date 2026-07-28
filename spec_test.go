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
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// specOutput runs s with its stdout connected to a pipe and returns
// everything the child wrote, exercising the non-frozen stdio path.
func specOutput(t *testing.T, s *Spec) string {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	var ps ProcessState
	runErr := s.Run(nil, pw, nil, &ps)
	pw.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, pr); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("Run() failed: %v", runErr)
	}
	return buf.String()
}

func TestSpecRun(t *testing.T) {
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
		"error: exit 3": {
			args:     []string{"-c", "exit 3"},
			wantCode: 3,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s, err := NewSpec("sh", tt.args, []string{}, "")
			if err != nil {
				t.Fatalf("NewSpec() failed: %v", err)
			}
			// Run the frozen command repeatedly: the Spec must be
			// reusable with no state bleed between runs.
			for i := range 3 {
				var ps ProcessState
				err := s.Run(nil, nil, nil, &ps)
				if tt.wantOK {
					if err != nil {
						t.Fatalf("Run() #%d failed: %v", i, err)
					}
				} else {
					var ee *ExitError
					if !errors.As(err, &ee) {
						t.Fatalf("Run() #%d = %v (%T), want *ExitError", i, err, err)
					}
				}
				if got := ps.ExitCode(); got != tt.wantCode {
					t.Fatalf("ExitCode() #%d = %d, want %d", i, got, tt.wantCode)
				}
				if ps.Pid() == 0 {
					t.Fatalf("ProcessState #%d has no pid", i)
				}
			}
		})
	}
}

func TestSpecOutputAndEnv(t *testing.T) {
	t.Parallel()

	s, err := NewSpec("sh", []string{"-c", `printf '%s' "$FASTEXEC_SPEC_TEST"`}, []string{"FASTEXEC_SPEC_TEST=frozen-env"}, "")
	if err != nil {
		t.Fatalf("NewSpec() failed: %v", err)
	}
	if diff := gocmp.Diff("frozen-env", specOutput(t, s)); diff != "" {
		t.Fatalf("frozen environment mismatch (-want +got):\n%s", diff)
	}
}

func TestSpecDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	s, err := NewSpec("pwd", nil, []string{}, dir)
	if err != nil {
		t.Fatalf("NewSpec() failed: %v", err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSuffix(specOutput(t, s), "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(resolved, got); diff != "" {
		t.Fatalf("working directory mismatch (-want +got):\n%s", diff)
	}
}

func TestSpecConcurrent(t *testing.T) {
	t.Parallel()

	s, err := NewSpec("sh", []string{"-c", "exit 0"}, []string{}, "")
	if err != nil {
		t.Fatalf("NewSpec() failed: %v", err)
	}
	const workers = 32
	var wg sync.WaitGroup
	errc := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			for range 8 {
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
		t.Errorf("concurrent Spec.Run() failed: %v", err)
	}
}

func TestSpecErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mk        func() (*Spec, error)
		wantNew   bool
		wantErrno syscall.Errno
	}{
		"error: PATH lookup failure at NewSpec": {
			mk: func() (*Spec, error) {
				return NewSpec("fastexec-test-definitely-not-a-command", nil, []string{}, "")
			},
			wantNew: true,
		},
		"error: NUL byte rejected at NewSpec": {
			mk: func() (*Spec, error) {
				return NewSpec("/bin/echo", []string{"a\x00b"}, []string{}, "")
			},
			wantNew:   true,
			wantErrno: syscall.EINVAL,
		},
		"error: exec failure surfaces errno from Run": {
			mk: func() (*Spec, error) {
				return NewSpec("/nonexistent/fastexec/binary", nil, []string{}, "")
			},
			wantErrno: syscall.ENOENT,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s, err := tt.mk()
			if tt.wantNew {
				if err == nil {
					t.Fatal("NewSpec() succeeded, want error")
				}
				if tt.wantErrno != 0 && !errors.Is(err, tt.wantErrno) {
					t.Fatalf("NewSpec() = %v, want %v", err, tt.wantErrno)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewSpec() failed: %v", err)
			}
			var ps ProcessState
			err = s.Run(nil, nil, nil, &ps)
			if err == nil {
				t.Fatal("Run() succeeded, want error")
			}
			var errno syscall.Errno
			if !errors.As(err, &errno) || errno != tt.wantErrno {
				t.Fatalf("Run() = %v, want %v", err, tt.wantErrno)
			}
		})
	}
}

// TestSpecMarshalingEquivalence proves the frozen arena is
// byte-identical to what the per-spawn Cmd path would marshal for the
// same command, so the two paths hand the kernel the same image.
func TestSpecMarshalingEquivalence(t *testing.T) {
	t.Parallel()

	argv := []string{"a", "b c", ""}
	env := []string{"K=V", "EMPTY="}
	s, err := NewSpec("/bin/echo", argv, env, "/tmp")
	if err != nil {
		t.Fatalf("NewSpec() failed: %v", err)
	}

	var cs cstrs
	if _, _, _, _, err := cs.build("/bin/echo", "/tmp", append([]string{"/bin/echo"}, argv...), env); err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(string(cs.buf), string(s.cs.buf)); diff != "" {
		t.Fatalf("arena mismatch (-cmd +spec):\n%s", diff)
	}
}
