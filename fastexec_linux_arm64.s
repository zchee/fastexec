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

// Linux/arm64 system call numbers.
#define SYS_dup3		24
#define SYS_chdir		49
#define SYS_write		64
#define SYS_exit_group		94
#define SYS_rt_sigprocmask	135
#define SYS_clone		220
#define SYS_execve		221
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
// The child returns from the SVC on its own stack (cloneArgs.stack) and
// must never touch the parent's frame: it runs the branch at child·
// below using only registers and raw system calls, and leaves via execve
// or exit_group. The parent is suspended by CLONE_VFORK until then, so
// the child's stores through R19 (the shared childState) are race-free.
TEXT ·clone3Spawn(SB), NOSPLIT, $0-40
	MOVD cargs+0(FP), R0
	MOVD size+8(FP), R1
	MOVD child+16(FP), R19 // preserved across SVC; the child path relies on it
	MOVD $SYS_clone3, R8
	SVC  $0
	CBZ  R0, intochild
	CMN  $4095, R0
	BCS  parenterr
	MOVD R0, pid+24(FP)
	MOVD ZR, errno+32(FP)
	RET

parenterr:
	NEG  R0, R0
	MOVD R0, errno+32(FP)
	MOVD ZR, pid+24(FP)
	RET

intochild:
	JMP ·childRun(SB)

// func cloneSpawn(flags, stackTop uintptr, pidfd *int32, child *childState) (pid, errno uintptr)
//
// Fallback for environments whose seccomp policy rejects clone3 (Docker's
// default profile returns ENOSYS for it). clone(2) with CLONE_PIDFD is
// equivalent for the flag set used here and available since Linux 5.2.
// arm64 clone argument order is (flags, newsp, parent_tidptr, tls,
// child_tidptr).
TEXT ·cloneSpawn(SB), NOSPLIT, $0-48
	MOVD flags+0(FP), R0
	MOVD stackTop+8(FP), R1
	MOVD pidfd+16(FP), R2   // parent_tidptr receives the pidfd with CLONE_PIDFD
	MOVD ZR, R3             // tls
	MOVD ZR, R4             // child_tidptr
	MOVD child+24(FP), R19  // preserved across SVC; the child path relies on it
	MOVD $SYS_clone, R8
	SVC  $0
	CBZ  R0, intochild2
	CMN  $4095, R0
	BCS  parenterr2
	MOVD R0, pid+32(FP)
	MOVD ZR, errno+40(FP)
	RET

parenterr2:
	NEG  R0, R0
	MOVD R0, errno+40(FP)
	MOVD ZR, pid+32(FP)
	RET

intochild2:
	JMP ·childRun(SB)

// childRun is the vfork child body shared by clone3Spawn and cloneSpawn.
// On entry R19 holds the childState pointer and RSP points into the
// private child stack; the parent's frame is never touched.
TEXT ·childRun(SB), NOSPLIT, $0
	// dup3(fds[0], 0, 0)
	MOVD CHILD_FD0(R19), R0
	MOVD ZR, R1
	MOVD ZR, R2
	MOVD $SYS_dup3, R8
	SVC  $0
	CMN  $4095, R0
	BCS  childfail

	// dup3(fds[1], 1, 0)
	MOVD CHILD_FD1(R19), R0
	MOVD $1, R1
	MOVD ZR, R2
	MOVD $SYS_dup3, R8
	SVC  $0
	CMN  $4095, R0
	BCS  childfail

	// dup3(fds[2], 2, 0)
	MOVD CHILD_FD2(R19), R0
	MOVD $2, R1
	MOVD ZR, R2
	MOVD $SYS_dup3, R8
	SVC  $0
	CMN  $4095, R0
	BCS  childfail

	// chdir(dir), if requested
	MOVD CHILD_DIR(R19), R0
	CBZ  R0, nochdir
	MOVD $SYS_chdir, R8
	SVC  $0
	CMN  $4095, R0
	BCS  childfail

nochdir:
	// rt_sigprocmask(SIG_SETMASK, &child.sigmask, NULL, 8): restore the
	// mask the parent saved before blocking all signals. Done last so no
	// Go signal handler can run in the child on the shared address space.
	MOVD $2, R0
	ADD  $CHILD_SIGMASK, R19, R1
	MOVD ZR, R2
	MOVD $8, R3
	MOVD $SYS_rt_sigprocmask, R8
	SVC  $0

	// execve(path, argv, envp)
	MOVD CHILD_PATH(R19), R0
	MOVD CHILD_ARGV(R19), R1
	MOVD CHILD_ENVP(R19), R2
	MOVD $SYS_execve, R8
	SVC  $0

	// Reached only if execve failed; R0 holds -errno.
childfail:
	NEG  R0, R0
	MOVD R0, CHILD_ERRNO(R19)

	// If an error pipe was set up, also report the errno through it for
	// kernels that do not honor CLONE_VM address-space sharing.
	MOVD CHILD_PIPEFD(R19), R0
	CBZ  R0, childexit
	ADD  $CHILD_ERRNO, R19, R1
	MOVD $8, R2
	MOVD $SYS_write, R8
	SVC  $0

childexit:
	MOVD $127, R0
	MOVD $SYS_exit_group, R8
	SVC  $0
	B    childexit

// func rawSigprocmask(how uintptr, set, old *uint64) (errno uintptr)
TEXT ·rawSigprocmask(SB), NOSPLIT, $0-32
	MOVD how+0(FP), R0
	MOVD set+8(FP), R1
	MOVD old+16(FP), R2
	MOVD $8, R3
	MOVD $SYS_rt_sigprocmask, R8
	SVC  $0
	CMN  $4095, R0
	BCS  maskerr
	MOVD ZR, errno+24(FP)
	RET

maskerr:
	NEG  R0, R0
	MOVD R0, errno+24(FP)
	RET
