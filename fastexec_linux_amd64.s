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

#include "textflag.h"

// Linux/amd64 system call numbers.
#define SYS_write		1
#define SYS_rt_sigprocmask	14
#define SYS_clone		56
#define SYS_execve		59
#define SYS_chdir		80
#define SYS_exit_group		231
#define SYS_dup3		292
#define SYS_clone3		435

// childState field offsets; must match childState in fastexec_linux.go.
#define CHILD_ERRNO	0
#define CHILD_DIR	8
#define CHILD_PATH	16
#define CHILD_ARGV	24
#define CHILD_ENVP	32
#define CHILD_FD0	40
#define CHILD_FD1	48
#define CHILD_FD2	56
#define CHILD_SIGMASK	64
#define CHILD_PIPEFD	72

// func clone3Spawn(cargs *cloneArgs, size uintptr, child *childState) (pid, errno uintptr)
//
// The child returns from the SYSCALL on its own stack (cloneArgs.stack)
// and must therefore never touch the parent's frame: it runs the branch
// at child· below using only registers and raw system calls, and leaves
// via execve or exit_group. The parent is suspended by CLONE_VFORK until
// then, so the child's stores through R12 (the shared childState) are
// race-free.
TEXT ·clone3Spawn(SB), NOSPLIT|NOFRAME, $0-40
	MOVQ  cargs+0(FP), DI
	MOVQ  size+8(FP), SI
	MOVQ  child+16(FP), R12       // preserved across SYSCALL; the child path relies on it
	MOVQ  $SYS_clone3, AX
	SYSCALL
	TESTQ AX, AX
	JEQ   intochild
	CMPQ  AX, $0xfffffffffffff001
	JHS   parenterr
	MOVQ  AX, pid+24(FP)
	MOVQ  $0, errno+32(FP)
	RET

parenterr:
	NEGQ AX
	MOVQ AX, errno+32(FP)
	MOVQ $0, pid+24(FP)
	RET

intochild:
	JMP ·childRun(SB)

// func cloneSpawn(flags, stackTop uintptr, pidfd *int32, child *childState) (pid, errno uintptr)
//
// Fallback for environments whose seccomp policy rejects clone3 (Docker's
// default profile returns ENOSYS for it). clone(2) with CLONE_PIDFD is
// equivalent for the flag set used here and available since Linux 5.2.
TEXT ·cloneSpawn(SB), NOSPLIT|NOFRAME, $0-48
	MOVQ  flags+0(FP), DI
	MOVQ  stackTop+8(FP), SI
	MOVQ  pidfd+16(FP), DX        // parent_tidptr receives the pidfd with CLONE_PIDFD
	XORL  R10, R10                // child_tidptr
	XORL  R8, R8                  // tls
	MOVQ  child+24(FP), R12       // preserved across SYSCALL; the child path relies on it
	MOVQ  $SYS_clone, AX
	SYSCALL
	TESTQ AX, AX
	JEQ   intochild2
	CMPQ  AX, $0xfffffffffffff001
	JHS   parenterr2
	MOVQ  AX, pid+32(FP)
	MOVQ  $0, errno+40(FP)
	RET

parenterr2:
	NEGQ AX
	MOVQ AX, errno+40(FP)
	MOVQ $0, pid+32(FP)
	RET

intochild2:
	JMP ·childRun(SB)

// childRun is the vfork child body shared by clone3Spawn and cloneSpawn.
// On entry R12 holds the childState pointer and SP points into the
// private child stack; the parent's frame is never touched.
TEXT ·childRun(SB), NOSPLIT|NOFRAME, $0
	// dup3(fds[0], 0, 0)
	MOVQ CHILD_FD0(R12), DI
	XORL SI, SI
	XORL DX, DX
	MOVQ $SYS_dup3, AX
	SYSCALL
	CMPQ AX, $0xfffffffffffff001
	JHS  childfail

	// dup3(fds[1], 1, 0)
	MOVQ CHILD_FD1(R12), DI
	MOVL $1, SI
	XORL DX, DX
	MOVQ $SYS_dup3, AX
	SYSCALL
	CMPQ AX, $0xfffffffffffff001
	JHS  childfail

	// dup3(fds[2], 2, 0)
	MOVQ CHILD_FD2(R12), DI
	MOVL $2, SI
	XORL DX, DX
	MOVQ $SYS_dup3, AX
	SYSCALL
	CMPQ AX, $0xfffffffffffff001
	JHS  childfail

	// chdir(dir), if requested
	MOVQ  CHILD_DIR(R12), DI
	TESTQ DI, DI
	JEQ   nochdir
	MOVQ  $SYS_chdir, AX
	SYSCALL
	CMPQ  AX, $0xfffffffffffff001
	JHS   childfail

nochdir:
	// rt_sigprocmask(SIG_SETMASK, &child.sigmask, NULL, 8): restore the
	// mask the parent saved before blocking all signals. Done last so no
	// Go signal handler can run in the child on the shared address space.
	MOVL $2, DI
	LEAQ CHILD_SIGMASK(R12), SI
	XORL DX, DX
	MOVQ $8, R10
	MOVQ $SYS_rt_sigprocmask, AX
	SYSCALL

	// execve(path, argv, envp)
	MOVQ CHILD_PATH(R12), DI
	MOVQ CHILD_ARGV(R12), SI
	MOVQ CHILD_ENVP(R12), DX
	MOVQ $SYS_execve, AX
	SYSCALL

	// Reached only if execve failed; AX holds -errno.
childfail:
	NEGQ AX
	MOVQ AX, CHILD_ERRNO(R12)

	// If an error pipe was set up, also report the errno through it for
	// kernels that do not honor CLONE_VM address-space sharing.
	MOVQ  CHILD_PIPEFD(R12), DI
	TESTQ DI, DI
	JEQ   childexit
	LEAQ  CHILD_ERRNO(R12), SI
	MOVQ  $8, DX
	MOVQ  $SYS_write, AX
	SYSCALL

childexit:
	MOVQ $127, DI
	MOVQ $SYS_exit_group, AX
	SYSCALL
	JMP  childexit

// func rawSigprocmask(how uintptr, set, old *uint64) (errno uintptr)
TEXT ·rawSigprocmask(SB), NOSPLIT|NOFRAME, $0-32
	MOVQ how+0(FP), DI
	MOVQ set+8(FP), SI
	MOVQ old+16(FP), DX
	MOVQ $8, R10
	MOVQ $SYS_rt_sigprocmask, AX
	SYSCALL
	CMPQ AX, $0xfffffffffffff001
	JHS  maskerr
	MOVQ $0, errno+24(FP)
	RET

maskerr:
	NEGQ AX
	MOVQ AX, errno+24(FP)
	RET
