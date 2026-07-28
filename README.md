# fastexec

[![Go Reference](https://pkg.go.dev/badge/github.com/zchee/fastexec.svg)](https://pkg.go.dev/github.com/zchee/fastexec)

Package `fastexec` is a high-throughput, low-latency alternative to `os/exec`
for spawning external processes on Linux and Darwin (macOS).

## Why

`os/exec` favors portability and conservative safety. Under high spawn rates
that conservatism becomes the bottleneck:

- Every spawn serializes on the global `syscall.ForkLock` (an exclusive
  `sync.RWMutex`), so spawn throughput *degrades* as parallelism increases.
- Argument and environment marshaling allocates a fresh NUL-terminated copy
  of every string on every spawn, pressuring the GC.
- `exec.CommandContext` cancellation signals the child by PID, which is
  subject to PID-reuse races on busy systems.
- On Darwin, `fork(2)` from a large multithreaded process risks
  `pthread_atfork` deadlocks and copy-on-write stalls; Apple explicitly
  discourages fork+exec in favor of `posix_spawn(2)`.

`fastexec` removes each of these costs with platform-specific
process-creation primitives while keeping an `os/exec`-shaped API.

## Design

### Linux (amd64, arm64)

- Processes are created with a raw `clone3(2)` system call using
  `CLONE_VFORK | CLONE_VM`: the parent's page tables are **never copied**,
  so spawn latency is O(1) in the parent's heap size.
- `CLONE_PIDFD` returns a pidfd atomically at creation. Waiting uses
  `waitid(P_PIDFD)` and signaling uses `pidfd_send_signal(2)`, so
  cancellation can **never** hit a recycled PID.
- `CLONE_CLEAR_SIGHAND` (Linux 5.5+) resets every signal disposition
  inside the child atomically, so no Go signal handler can ever run in the
  shared address space: the fast path performs **no signal masking and no
  thread pinning at all**. Kernels that reject the flag (5.4) latch a
  transparent fallback that blocks all signals across the vfork window and
  has the child restore the mask before exec — the pre-optimization
  behavior.
- The vfork child runs entirely in hand-written assembly (amd64 and arm64)
  on a private stack until `execve(2)` — no Go runtime code, no stack
  growth.
- Stdio sources already at fd >= 3 (the `/dev/null` singleton, `os.Pipe`
  ends, regular files) are handed to the child **without any dup-up**; the
  child's `dup3` onto 0–2 cannot collide because every source sits above
  the target range. Only sources at fds 0–2 are lifted with
  `F_DUPFD_CLOEXEC`. Non-CLOEXEC sources appear in the child at their
  original descriptor, exactly like `os/exec`.
- `syscall.ForkLock` is **not taken**, so spawns scale with CPU count. A
  steady-state spawn costs the parent **three system calls**: `clone3`,
  `waitid`, `close` (see the syscall table below).
- Environments whose seccomp policy rejects `clone3` (Docker's default
  profile returns `ENOSYS`) transparently fall back to `clone(2)` with the
  same flags. Kernels that do not honor `CLONE_VM` sharing for vfork
  children (observed on OrbStack) are detected with a one-time probe, and
  exec-failure reporting switches to a CLOEXEC pipe, exactly like
  `os/exec`.
- Requires Linux 5.4+ (`clone3`/`clone(CLONE_PIDFD)` plus
  `waitid(P_PIDFD)`); 5.5+ engages the no-masking fast path.

### Darwin (amd64, arm64)

- Processes are created with `posix_spawn(2)` — the only fork-safe
  primitive Apple supports for multithreaded programs. The kernel handles
  the intermediate child state, avoiding `pthread_atfork` deadlocks and
  copy-on-write stalls entirely.
- `posix_spawn` is reached through a direct libSystem call using the same
  trampoline mechanism as `golang.org/x/sys/unix` (dynamic import + Go
  runtime libc gate), **not cgo** — there is no cgo call overhead.
- The spawn attribute (`POSIX_SPAWN_SETSIGDEF | SETSIGMASK |
  CLOEXEC_DEFAULT` — identical for every spawn) is initialized **once per
  pooled spawn state** and destroyed by a `runtime.AddCleanup`, dropping
  five libc calls from every spawn. The remaining per-spawn bookkeeping
  (file-actions init/dup2/chdir/destroy) goes through `syscall.rawSyscall6`
  — no scheduler round trip; only the blocking `posix_spawn` itself pays
  `entersyscall`. The `Cmd` path makes 6 libc calls per spawn.
- A `Spec` (see below) with all-nil stdio freezes its file actions at
  construction: such a spawn is **one** `posix_spawn` libc call plus the
  `wait4` — the platform floor without reimplementing Apple's private
  `__posix_spawn` ABI.
- `POSIX_SPAWN_CLOEXEC_DEFAULT` guarantees that no descriptor other than
  the three mapped stdio descriptors reaches the child — even descriptors
  opened without `O_CLOEXEC` — with no `ForkLock`.

### Both platforms

- argv/envp/path strings are marshaled into a pooled, reusable arena as
  NUL-terminated C strings referenced by raw pointers: steady-state
  spawning performs **no per-string heap allocations**.
- `Cmd.Process` and `Cmd.ProcessState` are value fields, `(*Cmd).Reset`
  returns a Cmd to its pre-start state, and the nil-`Env` environment
  snapshot is cached process-wide (`InvalidateEnv` refreshes it): a reused
  `Cmd` with preset `Env` and every `Spec.Run` spawn **zero heap
  allocations** (`testing.AllocsPerRun == 0`, enforced in tests).
- `Spec` freezes PATH resolution, argv, env, and dir at construction into
  a retained arena; it is immutable and safe for concurrent use — built
  for hot loops and worker pools running one command many times.
- Stdio is `*os.File` only (nil means a shared `/dev/null`), so the hot
  path starts no goroutines and copies no data. `Output` and
  `CombinedOutput` build the pipes for you when convenience matters.

## Usage

```go
import "github.com/zchee/fastexec"

// Drop-in shape for the common cases.
out, err := fastexec.Command("git", "rev-parse", "HEAD").Output()

// Context cancellation kills through a pidfd on Linux: no PID-reuse race.
ctx, cancel := context.WithTimeout(context.Background(), time.Second)
defer cancel()
err = fastexec.CommandContext(ctx, "sleep", "30").Run()

// Zero-allocation respawn loop: preset Env, reuse the Cmd via Reset.
c := fastexec.Command("/usr/bin/worker")
c.Env = os.Environ()
for range jobs {
	if err := c.Run(); err != nil { ... }
	c.Reset()
}

// Spec: freeze the command once, spawn concurrently with 0 allocs/op.
spec, err := fastexec.NewSpec("/usr/bin/worker", []string{"--fast"}, nil, "")
if err != nil { ... }
var ps fastexec.ProcessState
if err := spec.Run(nil, nil, nil, &ps); err != nil { ... }

// After os.Setenv, refresh the cached nil-Env snapshot.
os.Setenv("MODE", "prod")
fastexec.InvalidateEnv()
```

## Benchmarks

Spawning `/usr/bin/true`, Go 1.26. `benchstat` over 10 runs; os/exec
numbers from the same sessions serve as drift control.

Apple M3 Max (Darwin arm64, 16 cores):

```
BenchmarkRunFastexec-16          1.402m ± 2%    419 B/op    2 allocs/op
BenchmarkRunFastexecReset-16     1.414m ± 2%      0 B/op    0 allocs/op
BenchmarkRunSpec-16              1.397m ± 3%      0 B/op    0 allocs/op
BenchmarkRunOsExec-16            2.469m ± 1%   1336 B/op   25 allocs/op
BenchmarkRunParallelSpec-16       149.0µ ±14%      0 B/op    0 allocs/op
BenchmarkRunParallelFastexec-16   157.3µ ±26%    419 B/op    2 allocs/op
BenchmarkRunParallelOsExec-16     485.0µ ± 1%   1399 B/op   25 allocs/op
```

Linux arm64 (lima VM, Ubuntu 6.17 kernel, 8 vCPU):

```
BenchmarkRunFastexec-8            220.2µ ± 2%    412 B/op    2 allocs/op
BenchmarkRunFastexecReset-8       219.2µ ± 2%      0 B/op    0 allocs/op
BenchmarkRunSpec-8                220.2µ ± 3%      0 B/op    0 allocs/op
BenchmarkRunOsExec-8              243.4µ ± 1%   1576 B/op   30 allocs/op
BenchmarkRunParallelSpec-8        34.19µ ± 1%      0 B/op    0 allocs/op
BenchmarkRunParallelFastexec-8    34.73µ ± 7%    415 B/op    2 allocs/op
BenchmarkRunParallelOsExec-8      44.84µ ± 5%   1576 B/op   30 allocs/op
```

1.8x (sequential) to 3.3x (parallel) faster on Darwin, 1.1–1.3x on Linux,
with zero steady-state allocations against 25–30 for `os/exec`.

### Parallel scaling (`-cpu` sweep, `Spec.Run` vs `os/exec`)

The gap widens with core count because `os/exec` serializes on
`ForkLock` while fastexec never takes it — on Darwin `os/exec` stops
scaling entirely past 8 cores:

| GOMAXPROCS | darwin Spec | darwin os/exec | ratio | linux Spec | linux os/exec | ratio |
|---:|---:|---:|---:|---:|---:|---:|
| 1  | 1447µs | 2441µs | 1.7x | 225µs | 246µs | 1.1x |
| 4  | 326µs  | 648µs  | 2.0x | 61.6µs | 68.2µs | 1.1x |
| 8  | 177µs  | 482µs  | 2.7x | 34.1µs | 44.7µs | 1.3x |
| 16 | 149µs  | 485µs  | 3.3x | —      | —      | —    |

### Large-heap parent (4 GiB touched ballast)

Darwin `os/exec` forks, so spawn cost grows with the parent's memory
image; `posix_spawn` does not: with a 4 GiB ballast `os/exec` degrades
2.44ms → 2.75ms (+12%) while `Spec.Run` stays within noise of its
no-ballast time (~1.46ms median). On Linux both are vfork-class and
neither degrades. (Run with `FASTEXEC_BENCH_BALLAST_GIB=4`.)

### Syscall profile

`strace -f -c` over 1000 warmed `Spec.Run` spawns (lima, Ubuntu
6.17.0-29 arm64) — the parent-side cost per spawn is exactly 3 syscalls
(`clone3` + `waitid` + `close`); `fcntl`, `pipe2`, and `rt_sigprocmask`
appear only as O(1) startup/probe totals:

```
     calls    errors syscall          per spawn
      1024        22 waitid           1 (parent)
      1002           clone3           1 (parent)
      3007           close            1 (parent) + 2 (child loader)
      3006           dup3             3 (child stdio wiring)
      1003         1 execve           1 (child)
        14           rt_sigprocmask   0 (runtime startup only)
        10           fcntl            0 (probe only)
         0           pipe2            0 (shared-memory errno mode)
```

An `exit_signal=0` experiment (suppressing the SIGCHLD delivered into
the Go runtime per child exit) produced no statistically significant
parallel win on a 6.17 kernel (p ≥ 0.14 across two A/B methodologies)
and was dropped per the optimization plan's measurement gate.

## Differences from os/exec

- `Stdin`/`Stdout`/`Stderr` are `*os.File`, not `io.Reader`/`io.Writer`;
  nil means `/dev/null`. Use `os.Pipe` for streaming.
- `Cmd.Process` and `Cmd.ProcessState` are **value fields**; a `Cmd` must
  not be copied after first use. `ExitError` embeds its own
  `ProcessState` copy and survives `Reset`.
- A `Cmd` is respawnable via `(*Cmd).Reset()` instead of being single-use.
- With nil `Env` the environment snapshot is **cached process-wide**;
  call `InvalidateEnv()` after `os.Setenv` and friends. Entries are
  passed to the kernel verbatim (no deduplication).
- Children always start with default signal dispositions and an empty
  signal mask.
- Process signaling is `Process.Signal(unix.Signal)` / `Process.Kill()`,
  through a pidfd on Linux.
- Unsupported `os/exec` features: `ExtraFiles`, `SysProcAttr`, `Cancel`,
  `WaitDelay`, and `StdinPipe`-style helpers.

`Cmd` methods must not be called concurrently; `Spec` is safe for
concurrent use.

### Environment knobs (testing/triage)

- `FASTEXEC_NO_CLEAR_SIGHAND=1` forces the Linux signal-mask fallback
  even on kernels that support `CLONE_CLEAR_SIGHAND`.

## Requirements

- Go 1.26+
- Linux 5.4+ on amd64 or arm64 (5.5+ engages the no-masking fast path),
  or any supported Darwin (macOS 10.15+ if `Cmd.Dir`/`NewSpec` dir is
  used, for `posix_spawn_file_actions_addchdir_np`)

## Acknowledgments

The design distills the optimization strategies surveyed in a deep-research
report on pushing `exec.Command` beyond its limits: vfork-class cloning to
avoid page-table duplication, ForkLock-free descriptor management on atomic
`O_CLOEXEC` kernels, arena-based zero-allocation argv/envp marshaling,
pidfd-based lifecycle management, and `posix_spawn` on Darwin.

## License

Apache License 2.0. See [LICENSE](LICENSE).
