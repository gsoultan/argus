/* Minimal kernel type declarations for the Argus execution probe.
 *
 * A full vmlinux.h is generated with `bpftool btf dump`, which needs a build
 * host whose kernel carries BTF. This declares only the types the probe reads,
 * which is enough because CO-RE resolves every field offset against the target
 * kernel's own BTF at load time — the definitions here supply names, not
 * layout. A struct may therefore omit every field the probe does not touch.
 *
 * preserve_access_index is what makes that true: without it clang would emit
 * fixed offsets from these definitions, which are wrong on every real kernel.
 */

#ifndef __VMLINUX_H__
#define __VMLINUX_H__

typedef signed char __s8;
typedef unsigned char __u8;
typedef short int __s16;
typedef short unsigned int __u16;
typedef int __s32;
typedef unsigned int __u32;
typedef long long int __s64;
typedef long long unsigned int __u64;

typedef __u8 u8;
typedef __u16 u16;
typedef __u32 u32;
typedef __u64 u64;
typedef __s32 s32;

typedef int pid_t;

/* Endian-tagged aliases used throughout the libbpf helper declarations. The
 * helpers are declared whether or not this program calls them, so the names
 * have to exist even though the probe touches none of them. */
typedef __u16 __be16;
typedef __u16 __le16;
typedef __u32 __be32;
typedef __u32 __le32;
typedef __u64 __be64;
typedef __u16 __sum16;
typedef __u32 __wsum;

/* BPF UAPI constants. Normally supplied by the generated vmlinux.h; the values
 * are ABI and must match include/uapi/linux/bpf.h. */
enum bpf_map_type {
	BPF_MAP_TYPE_UNSPEC = 0,
	BPF_MAP_TYPE_HASH = 1,
	BPF_MAP_TYPE_ARRAY = 2,
	BPF_MAP_TYPE_PERCPU_HASH = 5,
	BPF_MAP_TYPE_PERCPU_ARRAY = 6,
	BPF_MAP_TYPE_LRU_HASH = 9,
	BPF_MAP_TYPE_RINGBUF = 27,
};

enum {
	BPF_ANY = 0,
	BPF_NOEXIST = 1,
	BPF_EXIST = 2,
};

#ifndef NULL
#define NULL ((void *)0)
#endif

#ifndef offsetof
#define offsetof(TYPE, MEMBER) __builtin_offsetof(TYPE, MEMBER)
#endif

#pragma clang attribute push(__attribute__((preserve_access_index)), apply_to = record)

struct mm_struct {
	/* argv occupies [arg_start, arg_end) in the process's own address space. */
	long unsigned int arg_start;
	long unsigned int arg_end;
};

struct task_struct {
	int pid;
	int tgid;
	struct task_struct *real_parent;
	struct mm_struct *mm;
};

/* Tracepoint argument structures.
 *
 * These are ABI, not internal layout: the sched tracepoints have carried these
 * fields unchanged for many years, and their offsets are relocated by CO-RE in
 * any case.
 */

struct trace_entry {
	short unsigned int type;
	unsigned char flags;
	unsigned char preempt_count;
	int pid;
};

struct trace_event_raw_sched_process_fork {
	struct trace_entry ent;
	char parent_comm[16];
	pid_t parent_pid;
	char child_comm[16];
	pid_t child_pid;
	char __data[0];
};

struct trace_event_raw_sched_process_exec {
	struct trace_entry ent;
	/* Variable-length field: the low 16 bits hold the offset from the start
	 * of this structure to the filename, the high 16 bits its length. */
	u32 __data_loc_filename;
	pid_t pid;
	pid_t old_pid;
	char __data[0];
};

struct trace_event_raw_sched_process_template {
	struct trace_entry ent;
	char comm[16];
	pid_t pid;
	int prio;
	char __data[0];
};

#pragma clang attribute pop

#endif /* __VMLINUX_H__ */
