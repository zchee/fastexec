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

TEXT libc_posix_spawn_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_posix_spawn(SB)

GLOBL ·libc_posix_spawn_trampoline_addr(SB), RODATA, $8
DATA ·libc_posix_spawn_trampoline_addr(SB)/8, $libc_posix_spawn_trampoline<>(SB)

TEXT libc_posix_spawnattr_init_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_posix_spawnattr_init(SB)

GLOBL ·libc_posix_spawnattr_init_trampoline_addr(SB), RODATA, $8
DATA ·libc_posix_spawnattr_init_trampoline_addr(SB)/8, $libc_posix_spawnattr_init_trampoline<>(SB)

TEXT libc_posix_spawnattr_destroy_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_posix_spawnattr_destroy(SB)

GLOBL ·libc_posix_spawnattr_destroy_trampoline_addr(SB), RODATA, $8
DATA ·libc_posix_spawnattr_destroy_trampoline_addr(SB)/8, $libc_posix_spawnattr_destroy_trampoline<>(SB)

TEXT libc_posix_spawnattr_setflags_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_posix_spawnattr_setflags(SB)

GLOBL ·libc_posix_spawnattr_setflags_trampoline_addr(SB), RODATA, $8
DATA ·libc_posix_spawnattr_setflags_trampoline_addr(SB)/8, $libc_posix_spawnattr_setflags_trampoline<>(SB)

TEXT libc_posix_spawnattr_setsigdefault_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_posix_spawnattr_setsigdefault(SB)

GLOBL ·libc_posix_spawnattr_setsigdefault_trampoline_addr(SB), RODATA, $8
DATA ·libc_posix_spawnattr_setsigdefault_trampoline_addr(SB)/8, $libc_posix_spawnattr_setsigdefault_trampoline<>(SB)

TEXT libc_posix_spawnattr_setsigmask_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_posix_spawnattr_setsigmask(SB)

GLOBL ·libc_posix_spawnattr_setsigmask_trampoline_addr(SB), RODATA, $8
DATA ·libc_posix_spawnattr_setsigmask_trampoline_addr(SB)/8, $libc_posix_spawnattr_setsigmask_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_init_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_posix_spawn_file_actions_init(SB)

GLOBL ·libc_posix_spawn_file_actions_init_trampoline_addr(SB), RODATA, $8
DATA ·libc_posix_spawn_file_actions_init_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_init_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_destroy_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_posix_spawn_file_actions_destroy(SB)

GLOBL ·libc_posix_spawn_file_actions_destroy_trampoline_addr(SB), RODATA, $8
DATA ·libc_posix_spawn_file_actions_destroy_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_destroy_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_adddup2_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_posix_spawn_file_actions_adddup2(SB)

GLOBL ·libc_posix_spawn_file_actions_adddup2_trampoline_addr(SB), RODATA, $8
DATA ·libc_posix_spawn_file_actions_adddup2_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_adddup2_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_addchdir_np_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_posix_spawn_file_actions_addchdir_np(SB)

GLOBL ·libc_posix_spawn_file_actions_addchdir_np_trampoline_addr(SB), RODATA, $8
DATA ·libc_posix_spawn_file_actions_addchdir_np_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_addchdir_np_trampoline<>(SB)
