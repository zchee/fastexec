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
  so spawn latency is O(1) in the parent's heap size instead of degrading
  with process growth.
- `CLONE_PIDFD` returns a pidfd atomically at creation. Waiting uses
  `waitid(P_PIDFD)` and signaling uses `pidfd_send_signal(2)`, so
  cancellation can **never** hit a recycled PID.
- The vfork child runs entirely in hand-written assembly (amd64 and arm64)
  on a private stack until `execve(2)` — no Go runtime code, no stack
  growth, no signal handlers (all signals are masked across the vfork
  window and restored by the child immediately before exec).
- `syscall.ForkLock` is **not taken**. Descriptor handling relies solely on
  atomic `O_CLOEXEC` operations (`F_DUPFD_CLOEXEC`, `pipe2`), so spawns
  scale linearly with CPU count.
- Environments whose seccomp policy rejects `clone3` (Docker's default
  profile returns `ENOSYS`) transparently fall back to `clone(2)` with the
  same flags. Kernels that do not honor `CLONE_VM` sharing for vfork
  children (observed on OrbStack's kernel, where even glibc's
  `posix_spawn` loses the child's exec errno) are detected with a one-time
  probe, and exec-failure reporting switches to a CLOEXEC pipe, exactly
  like `os/exec`.
- Requires Linux 5.4+ (`clone3`/`clone(CLONE_PIDFD)` plus `waitid(P_PIDFD)`).

### Darwin (amd64, arm64)

- Processes are created with `posix_spawn(2)` — the only fork-safe
  primitive Apple supports for multithreaded programs. The kernel handles
  the intermediate child state, avoiding `pthread_atfork` deadlocks and
  copy-on-write stalls entirely.
- `posix_spawn` is reached through a direct libSystem call using the same
  trampoline mechanism as `golang.org/x/sys/unix` (dynamic import + Go
  runtime libc gate), **not cgo** — there is no cgo call overhead.
- `POSIX_SPAWN_CLOEXEC_DEFAULT` guarantees that no descriptor other than
  the three requested stdio descriptors reaches the child — even
  descriptors that were opened without `O_CLOEXEC` — with no `ForkLock`.
- `POSIX_SPAWN_SETSIGDEF | POSIX_SPAWN_SETSIGMASK` hand the child default
  signal dispositions and an empty mask, so the Go runtime's signal
  handlers never leak into children.

### Both platforms

- argv/envp/path strings are marshaled into a pooled, reusable arena as
  NUL-terminated C strings referenced by raw pointers
  (`unsafe.Pointer`-based, `sync.Pool`-recycled): steady-state spawning
  performs **no per-string heap allocations**.
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

// Zero-allocation hot loop: preset Env, reuse files.
env := os.Environ()
for _, job := range jobs {
	c := fastexec.Command("/usr/bin/worker", job)
	c.Env = env // avoids the os.Environ() copy per spawn
	if err := c.Run(); err != nil { ... }
}
```

## Benchmarks

Spawning `/usr/bin/true`, Go 1.26.

Apple M-series (Darwin arm64, 16 cores):

```
BenchmarkRunFastexec-16                 1070300 ns/op     430 B/op     4 allocs/op
BenchmarkRunOsExec-16                   1600383 ns/op    1336 B/op    25 allocs/op
BenchmarkRunParallelFastexec-16          115910 ns/op     787 B/op     4 allocs/op
BenchmarkRunParallelOsExec-16            388476 ns/op    1399 B/op    25 allocs/op
```

Linux arm64 (Ubuntu 6.17 kernel, 8 vCPU VM):

```
BenchmarkRunFastexec-8                   181264 ns/op     642 B/op     4 allocs/op
BenchmarkRunOsExec-8                     206209 ns/op    1577 B/op    30 allocs/op
BenchmarkRunParallelFastexec-8            28087 ns/op    2010 B/op     4 allocs/op
BenchmarkRunParallelOsExec-8              39107 ns/op    1597 B/op    30 allocs/op
```

Roughly 1.5x (sequential) to 3.4x (parallel) faster on Darwin and 1.1–1.4x
on Linux, with ~6x fewer allocations per spawn. The parallel gap widens
with core count because `os/exec` serializes on `ForkLock` while fastexec
does not. The sequential Linux gap also grows with the parent's memory
footprint for `os/exec`'s non-vfork fallback paths, while fastexec stays
O(1) by construction.

## Differences from os/exec

- `Stdin`/`Stdout`/`Stderr` are `*os.File`, not `io.Reader`/`io.Writer`;
  nil means `/dev/null`. Use `os.Pipe` for streaming.
- Environment entries are passed to the kernel verbatim (no
  deduplication).
- Children always start with default signal dispositions and an empty
  signal mask.
- Process signaling is `Process.Signal(syscall.Signal)` / `Process.Kill()`.
- Unsupported `os/exec` features: `ExtraFiles`, `SysProcAttr`, `Cancel`,
  `WaitDelay`, and `StdinPipe`-style helpers.

Like `os/exec`, a `Cmd` is single-use and its methods must not be called
concurrently.

## Requirements

- Go 1.26+
- Linux 5.4+ on amd64 or arm64, or any supported Darwin (macOS 10.15+ if
  `Cmd.Dir` is used, for `posix_spawn_file_actions_addchdir_np`)

## Acknowledgments

The design distills the optimization strategies surveyed in a deep-research
report on pushing `exec.Command` beyond its limits: vfork-class cloning to
avoid page-table duplication, ForkLock-free descriptor management on atomic
`O_CLOEXEC` kernels, arena-based zero-allocation argv/envp marshaling,
pidfd-based lifecycle management, and `posix_spawn` on Darwin.

## License

Apache License 2.0. See [LICENSE](LICENSE).
