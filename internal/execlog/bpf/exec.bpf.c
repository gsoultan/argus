// Kernel-side execution tracing for Argus.
//
// Reports every process execution belonging to a recorded session, so an
// auditor sees what ran rather than what the terminal displayed. A user can
// base64 a command, source a script whose body never appears on screen, or
// drive an editor that shells out; none of that is visible in a PTY recording
// and all of it is visible here.
//
// Attribution is by inherited session key rather than by walking the process
// tree at exec time. Userspace seeds the map with the session leader's PID; a
// fork copies the key to the child. Inheritance happens at fork, so a process
// that is later re-parented — every daemonised process a user starts — keeps
// the attribution it was born with. Walking parents at exec time would lose
// exactly those, which are the ones worth catching.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

#define ARGS_BUF_SIZE 4096
#define MAX_TRACKED   10240
/* A canonical UUID is 36 characters, plus a terminator. It was 33, sized for
 * the bare hex ids the gateway used to mint; when those became canonical UUIDs
 * the userspace side began refusing to track any session at all, because a
 * 36-character id does not fit. Refusing was the right failure -- a truncated
 * id attributes an execution to a session that does not exist -- but the
 * kernel evidence tier was off until this matched. */
#define SESSION_LEN   37
#define COMM_LEN      16
#define FILENAME_LEN  256

struct session_key {
    char id[SESSION_LEN];
};

// pid -> session. Bounded: a map keyed by something the workload controls needs
// a ceiling, and 10k concurrent tracked processes is far past any plausible
// interactive session count.
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, MAX_TRACKED);
    __type(key, __u32);
    __type(value, struct session_key);
} tracked SEC(".maps");

// session -> executions the ring buffer refused.
//
// Pre-created by Track, so the path below only ever looks up and adds: creating
// an entry here could itself fail, and a counter of lost evidence that can be
// lost is not a counter.
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, MAX_TRACKED);
    __type(key, struct session_key);
    __type(value, __u64);
} drops SEC(".maps");

struct exec_event {
    __u32 pid;
    __u32 ppid;
    __u32 uid;
    __u32 args_len;
    __u8  truncated;
    char  session[SESSION_LEN];
    char  comm[COMM_LEN];
    char  filename[FILENAME_LEN];
    char  args[ARGS_BUF_SIZE];
};

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 22); /* 4 MiB */
} events SEC(".maps");

// The event is far too large for the 512-byte stack, so it is assembled in a
// per-CPU scratch buffer and copied into the ring buffer once.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct exec_event);
} scratch SEC(".maps");

// A child inherits its parent's session at fork.
SEC("tracepoint/sched/sched_process_fork")
int handle_fork(struct trace_event_raw_sched_process_fork *ctx)
{
    __u32 parent = (__u32)ctx->parent_pid;
    __u32 child  = (__u32)ctx->child_pid;

    struct session_key *sk = bpf_map_lookup_elem(&tracked, &parent);
    if (!sk) {
        // A thread of a tracked process forking: the tracepoint reports the
        // creating thread, whose tid is not the tgid the map is keyed by.
        __u32 tgid = (__u32)(bpf_get_current_pid_tgid() >> 32);
        sk = bpf_map_lookup_elem(&tracked, &tgid);
        if (!sk)
            return 0;
    }

    if (bpf_map_update_elem(&tracked, &child, sk, BPF_ANY) != 0) {
        // The map is full. This child, and everything it goes on to run, will
        // not be attributed to the session -- which is the silent gap the exit
        // handler below calls a correctness requirement rather than
        // housekeeping. Charged to the session that lost it, so the recording
        // stops claiming to list everything that ran instead of quietly
        // omitting a subtree.
        __u64 *lost = bpf_map_lookup_elem(&drops, sk);
        if (lost)
            __sync_fetch_and_add(lost, 1);
    }
    return 0;
}

SEC("tracepoint/sched/sched_process_exec")
int handle_exec(struct trace_event_raw_sched_process_exec *ctx)
{
    __u32 pid = (__u32)(bpf_get_current_pid_tgid() >> 32);

    struct session_key *sk = bpf_map_lookup_elem(&tracked, &pid);
    if (!sk)
        return 0; /* not part of any recorded session */

    __u32 zero = 0;
    struct exec_event *e = bpf_map_lookup_elem(&scratch, &zero);
    if (!e)
        return 0;

    e->pid = pid;
    e->uid = (__u32)bpf_get_current_uid_gid();
    e->truncated = 0;
    __builtin_memcpy(e->session, sk->id, SESSION_LEN);
    bpf_get_current_comm(&e->comm, sizeof(e->comm));

    struct task_struct *task = (struct task_struct *)bpf_get_current_task();
    e->ppid = (__u32)BPF_CORE_READ(task, real_parent, tgid);

    // The tracepoint carries the filename as a variable-length field whose
    // offset lives in the top 16 bits of __data_loc_filename.
    unsigned short off = ctx->__data_loc_filename & 0xFFFF;
    bpf_probe_read_kernel_str(e->filename, sizeof(e->filename), (void *)ctx + off);

    // argv lives in userspace between mm->arg_start and mm->arg_end as a run of
    // NUL-separated strings. Reading it here, rather than from sys_enter_execve,
    // means only executions that actually succeeded are reported — a failed
    // exec in the audit log would misrepresent what the session did.
    unsigned long arg_start = BPF_CORE_READ(task, mm, arg_start);
    unsigned long arg_end   = BPF_CORE_READ(task, mm, arg_end);

    __u64 len = arg_end - arg_start;
    if (len > ARGS_BUF_SIZE) {
        len = ARGS_BUF_SIZE;
        // Say so rather than presenting a cut-off command as the whole thing.
        e->truncated = 1;
    }
    // The verifier needs the bound restated against the buffer size.
    if (len > 0 && len <= ARGS_BUF_SIZE) {
        long n = bpf_probe_read_user(e->args, len, (void *)arg_start);
        if (n == 0) {
            e->args_len = (__u32)len;
        } else {
            // argv lives in userspace and this helper does not fault pages in,
            // so a read can fail on memory that is simply not resident. Saying
            // args_len = 0 and nothing else reports a command that had no
            // arguments, which for `bash -c '...'` is the difference between
            // recording a shell and recording what it was told to run. Flagged
            // rather than asserted: not having the arguments and there being
            // none are not the same fact.
            e->args_len = 0;
            e->truncated = 1;
        }
    } else {
        e->args_len = 0;
    }

    // Only the bytes actually used are published; the scratch buffer is 4 KiB
    // and copying all of it per exec would waste most of the ring buffer.
    __u64 payload = offsetof(struct exec_event, args) + e->args_len;
    if (bpf_ringbuf_output(&events, e, payload, 0) != 0) {
        // The ring buffer is full and this execution is gone. Counted, because
        // eBPF fidelity claims every execve is in the recording and one that
        // never left the kernel makes that false -- silently, which is the part
        // that matters. The session is reported at reduced fidelity instead.
        __u64 *lost = bpf_map_lookup_elem(&drops, sk);
        if (lost)
            __sync_fetch_and_add(lost, 1);
    }
    return 0;
}

// Removing dead PIDs is a correctness requirement, not housekeeping. Linux
// recycles PIDs, so a stale entry would eventually attribute an unrelated
// process to a session that ended long ago — evidence against the wrong person.
SEC("tracepoint/sched/sched_process_exit")
int handle_exit(struct trace_event_raw_sched_process_template *ctx)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();

    // By tid, always -- never by tgid on a thread exit.
    //
    // Deleting by tgid here would remove a live session the moment its process
    // retired a worker thread, and a Go or Java shell helper does that
    // constantly: tracking would stop almost immediately and nothing would be
    // reported. That is why this used to return early for a thread.
    //
    // But returning early leaked. handle_fork inserts ctx->child_pid, and for a
    // thread clone that is a tid, not a tgid; handle_exec only ever looks up by
    // tgid, so the entry was never useful and was never removed either.
    // Measured: 64 threads created and joined left 65 entries behind. At
    // MAX_TRACKED the map stops accepting, new children go untracked, and the
    // execve evidence stops with nothing saying so.
    //
    // Deleting by tid does both jobs. A worker thread removes only its own
    // stray entry; the group leader's tid is its tgid, so its exit still
    // removes the session.
    bpf_map_delete_elem(&tracked, &tid);
    return 0;
}
