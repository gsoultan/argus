//go:build linux

package execlog

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// bpfObject is the compiled probe, built by scripts/build-bpf.sh.
//
// Committed rather than compiled during `go build`: the BPF target needs an
// LLVM most machines do not have, and requiring it would make the agent
// unbuildable almost everywhere it needs to be built. One object serves every
// architecture Argus supports — the probe uses only tracepoints, so it contains
// no architecture-specific register access, and x86-64 and arm64 are both
// little-endian.
//
//go:embed bpf/exec.bpf.o
var bpfObject []byte

type linuxProbe struct {
	coll    *ebpf.Collection
	links   []link.Link
	reader  *ringbuf.Reader
	tracked *ebpf.Map
	events  chan Exec
	log     *slog.Logger

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// Open loads and attaches the kernel probe.
//
// Every failure that means "this kernel cannot do it" is reported as
// ErrUnsupported with a reason an operator can act on, and separated from
// failures that mean something broke. The distinction matters: a host that
// cannot support kernel tracing is a supported deployment, and a host where it
// stopped working is an incident.
func Open(ctx context.Context, log *slog.Logger) (Probe, error) {
	if log == nil {
		log = slog.Default()
	}

	// Locked-memory limits are the most common reason loading fails on an
	// otherwise capable host, and the error the kernel returns for it is
	// unhelpful on its own.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, Unsupported("cannot raise the locked-memory limit: %v", err)
	}

	if _, err := os.Stat("/sys/kernel/btf/vmlinux"); err != nil {
		// Without BTF the CO-RE relocations in the object cannot be resolved,
		// so the program would be compiled against field offsets that do not
		// match this kernel. Refusing is the only safe answer.
		return nil, Unsupported(
			"this kernel was built without BTF (/sys/kernel/btf/vmlinux is absent), " +
				"so kernel-observed execution is not available; " +
				"a distribution kernel with CONFIG_DEBUG_INFO_BTF=y is required")
	}

	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(bpfObject))
	if err != nil {
		return nil, fmt.Errorf("parse probe object: %w", err)
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			// The verifier's own log is the only useful diagnostic here, and it
			// is lost if the error is wrapped plainly.
			return nil, fmt.Errorf("the kernel verifier rejected the probe: %w", ve)
		}
		return nil, Unsupported("cannot load the probe into this kernel: %v", err)
	}

	p := &linuxProbe{
		coll:   coll,
		events: make(chan Exec, 1024),
		log:    log,
		done:   make(chan struct{}),
	}

	// Attach exit first and fork second, so a process cannot be born into the
	// tracked map without something watching for it to leave. The reverse order
	// leaves a window in which an entry is created and never removed, and a
	// stale entry outlives its PID.
	for _, a := range []struct{ tp, prog string }{
		{"sched_process_exit", "handle_exit"},
		{"sched_process_fork", "handle_fork"},
		{"sched_process_exec", "handle_exec"},
	} {
		prog, ok := coll.Programs[a.prog]
		if !ok {
			p.closeResources()
			return nil, fmt.Errorf("probe object has no program %q", a.prog)
		}
		l, err := link.Tracepoint("sched", a.tp, prog, nil)
		if err != nil {
			p.closeResources()
			return nil, Unsupported("cannot attach to sched/%s: %v", a.tp, err)
		}
		p.links = append(p.links, l)
	}

	p.tracked = coll.Maps["tracked"]
	if p.tracked == nil {
		p.closeResources()
		return nil, errors.New("probe object has no tracked map")
	}

	rd, err := ringbuf.NewReader(coll.Maps["events"])
	if err != nil {
		p.closeResources()
		return nil, fmt.Errorf("open ring buffer: %w", err)
	}
	p.reader = rd

	p.wg.Add(1)
	go p.read()

	log.Info("kernel execution tracing active",
		"detail", "commands are observed at execve, not inferred from terminal output")
	return p, nil
}

// Track begins attributing pid and its descendants to sessionID.
func (p *linuxProbe) Track(pid int, sessionID string) error {
	if len(sessionID) >= sessionLen {
		return fmt.Errorf("session id %q does not fit in %d bytes", sessionID, sessionLen-1)
	}
	var key [sessionLen]byte
	copy(key[:], sessionID)
	if err := p.tracked.Put(uint32(pid), key); err != nil {
		// A full map means executions would go unattributed from here on, which
		// the caller must be able to refuse rather than discover in an audit.
		return fmt.Errorf("track pid %d: %w", pid, err)
	}
	return nil
}

// Untrack stops attributing a process.
//
// Descendants keep their own entries: they were given the session at fork and
// removing the parent must not orphan a shell that is still running. The kernel
// side deletes each entry when its process exits.
func (p *linuxProbe) Untrack(pid int) {
	if err := p.tracked.Delete(uint32(pid)); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		p.log.Warn("untrack failed", "pid", pid, "error", err)
	}
}

func (p *linuxProbe) Events() <-chan Exec { return p.events }

func (p *linuxProbe) read() {
	defer p.wg.Done()
	defer close(p.events)

	for {
		rec, err := p.reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			select {
			case <-p.done:
				return
			default:
			}
			p.log.Warn("ring buffer read failed", "error", err)
			continue
		}

		e, err := decodeEvent(rec.RawSample)
		if err != nil {
			// A malformed record means the Go layout and the C struct have
			// diverged. Reporting it is essential: the alternative is emitting
			// convincing nonsense.
			p.log.Error("undecodable execution event; the probe layout may be stale",
				"error", err, "bytes", len(rec.RawSample))
			continue
		}

		select {
		case p.events <- e:
		case <-p.done:
			return
		default:
			// Dropping is preferable to blocking the ring buffer reader, which
			// would cause the kernel to drop far more. Said out loud because a
			// gap in kernel evidence must never be silent.
			p.log.Warn("execution event dropped; the consumer is not keeping up",
				"session", e.SessionID, "command", e.CommandLine())
		}
	}
}

func (p *linuxProbe) closeResources() {
	for _, l := range p.links {
		_ = l.Close()
	}
	p.links = nil
	if p.reader != nil {
		_ = p.reader.Close()
	}
	if p.coll != nil {
		p.coll.Close()
	}
}

func (p *linuxProbe) Close() error {
	p.closeOnce.Do(func() {
		close(p.done)
		// Closing the reader is what unblocks Read; the goroutine then closes
		// the event channel on its way out.
		if p.reader != nil {
			_ = p.reader.Close()
		}
		p.wg.Wait()
		p.closeResources()
	})
	return nil
}
