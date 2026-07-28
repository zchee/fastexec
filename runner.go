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
	"path/filepath"
)

// Runner is a frozen command: the path (resolved through PATH once, like
// [Command]), argv, environment, and working directory are marshaled
// into a retained arena at construction, so spawning performs no PATH
// lookup, no marshaling, and no heap allocation at all. On Darwin a
// Runner spawned with all-nil stdio additionally uses a file-actions
// object frozen at construction, reducing the spawn to a single
// posix_spawn libc call.
//
// Unlike a [Cmd], a Runner is immutable after construction and safe for
// concurrent use by multiple goroutines. It is intended for hot loops
// and worker pools that run the same command many times.
type Runner struct {
	path  string
	cs    cstrs
	pathp *byte
	dirp  *byte
	argvp **byte
	envp  **byte
	os    runnerOS
}

// NewRunner freezes the named program with the given arguments,
// environment, and working directory.
//
// Name resolution follows [Command]: a name without path separators is
// resolved through PATH once, here. args does not include argv[0]; the
// name is used, as with [Command]. A nil env freezes a snapshot of the
// current environment (via the same cache as a nil Cmd.Env); an empty
// dir inherits the parent's working directory.
func NewRunner(name string, args []string, env []string, dir string) (*Runner, error) {
	path := name
	if filepath.Base(name) == name {
		lp, err := exec.LookPath(name)
		if err != nil {
			return nil, &Error{Name: name, Err: err}
		}
		path = lp
	}
	if env == nil {
		env = defaultEnv()
	}
	argv := make([]string, 0, len(args)+1)
	argv = append(argv, name)
	argv = append(argv, args...)

	s := &Runner{path: path}
	var err error
	s.pathp, s.dirp, s.argvp, s.envp, err = s.cs.build(path, dir, argv, env)
	if err != nil {
		return nil, &Error{Name: path, Err: err}
	}
	if err := s.freeze(); err != nil {
		return nil, err
	}
	return s, nil
}

// stdio resolves nil streams to the shared /dev/null descriptor.
func (s *Runner) stdio(stdin, stdout, stderr *os.File) ([3]*os.File, error) {
	files := [3]*os.File{stdin, stdout, stderr}
	for i, f := range files {
		if f == nil {
			null, err := devNull()
			if err != nil {
				return files, &Error{Name: s.path, Err: err}
			}
			files[i] = null
		}
	}
	return files, nil
}

// Start spawns the frozen command with the given stdio streams (nil
// means /dev/null), filling in p, which must be in its zero or reset
// state. After a successful Start the caller must call [Process.Wait]
// to reap the child and release its resources. Callers that never
// signal the child should prefer [Runner.Run], which bypasses the
// Process handle entirely.
func (s *Runner) Start(stdin, stdout, stderr *os.File, p *Process) error {
	files, err := s.stdio(stdin, stdout, stderr)
	if err != nil {
		return err
	}
	return startRunner(s, files, p)
}

// Run spawns the frozen command, waits for it to complete, and stores
// the final state into ps. Like [Cmd.Run] it returns an [*ExitError]
// for an unsuccessful exit.
//
// Run reaps the child directly rather than through a [Process] handle
// (whose signal-safety mutex would force a heap allocation), so it
// performs zero heap allocations on the success path.
func (s *Runner) Run(stdin, stdout, stderr *os.File, ps *ProcessState) error {
	files, err := s.stdio(stdin, stdout, stderr)
	if err != nil {
		return err
	}
	if err := runRunner(s, files, ps); err != nil {
		return err
	}
	if !ps.Success() {
		return &ExitError{ProcessState: *ps}
	}
	return nil
}
