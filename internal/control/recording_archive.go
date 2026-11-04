package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/storage"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The recording archive.

Recordings are kept for ever, and almost nobody replays one that is more
than a few weeks old. Once a recording is older than the archive age
(Settings › Recordings, 30 days by default), the control plane rewrites it at
zstd's strongest setting with a 128 MB window, which finds the screens and
output a session repeated minutes apart, and moves it under archive/ in the
same bucket or directory, in the bucket's archive storage class when one is
set. A desktop recording shrinks about half again on top of the compression
it was stored with.

It is still a recording like any other: the session row names the archived
object, the console replays it from there, and every read verifies its chain
as before. Nothing about the evidence changes, so the order is what matters:

 1. the stored copy is read and recompressed into a temporary file;
 2. that file is verified against the session's chain head -- the copy that
    will be kept is the one checked, so a stored recording that no longer
    verifies is never archived, and is reported instead;
 3. it is stored under archive/;
 4. the session row is moved to it, only if it still names the old key, so
    two replicas archiving the same session cannot both act on it;
 5. the old object is deleted, and the move is in the audit chain.

A crash between any two steps leaves the session naming an object that
exists and verifies. At worst an archived copy is stored and not yet named
(the next pass stores it again, the same bytes), or the old copy outlives the
move as an orphan a backup still keeps -- never a session pointing at
nothing.

Only instant-retrieval storage classes are allowed: a recording in Glacier or
Deep Archive could not be opened from the console without a restore that
takes hours, and "can still be replayed" is the point of this tier.
*/

// DefaultArchiveAfter is when a recording is archived when the setting says
// nothing.
const DefaultArchiveAfter = 30 * 24 * time.Hour

// archiveBatch bounds one organization's work in one pass.
const archiveBatch = 50

// ArchiveStorageClasses are the storage classes the archive may use: those a
// recording can be read back from at once.
var ArchiveStorageClasses = []string{"", "STANDARD", "STANDARD_IA", "ONEZONE_IA",
	"INTELLIGENT_TIERING", "GLACIER_IR"}

// ValidArchiveStorageClass reports whether c is one of ArchiveStorageClasses.
func ValidArchiveStorageClass(c string) bool {
	for _, v := range ArchiveStorageClasses {
		if c == v {
			return true
		}
	}
	return false
}

// RunRecordingArchive archives recordings older than ArchiveAfter until ctx
// ends: shortly after start, then every interval.
func (a *API) RunRecordingArchive(ctx context.Context, interval time.Duration) {
	t := time.NewTimer(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n, err := a.ArchiveRecordings(ctx, time.Now()); err != nil {
			a.log.Error("archiving recordings failed", "error", err)
		} else if n > 0 {
			a.log.Info("archived recordings", "count", n)
		}
		t.Reset(interval)
	}
}

// ArchiveRecordings archives, in every organization, the recordings that
// started before now less ArchiveAfter. It returns how many it moved.
func (a *API) ArchiveRecordings(ctx context.Context, now time.Time) (int, error) {
	if a.ArchiveAfter <= 0 || a.recordings == nil {
		return 0, nil
	}
	cutoff := now.Add(-a.ArchiveAfter)
	moved := 0
	err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
		list, err := a.store.RecordingsToArchive(tctx, cutoff, archiveBatch)
		if err != nil {
			a.log.Error("listing recordings to archive failed", "error", err)
			return
		}
		for _, c := range list {
			if ctx.Err() != nil {
				return
			}
			ok, err := a.archiveRecording(tctx, c)
			if err != nil {
				a.log.Warn("could not archive a recording; it stays where it is",
					"session", c.ID, "key", c.Key, "error", err)
				continue
			}
			if ok {
				moved++
			}
		}
	})
	return moved, err
}

// archiveCandidate is a stored recording old enough to archive.
type archiveCandidate struct {
	ID        string
	Key       string
	ChainHead string
	StartedAt time.Time
	Host      string
}

// errArchiveUnverified is a stored recording that does not verify. It is
// left where it is, as found.
var errArchiveUnverified = errors.New("the stored recording does not verify against its chain head")

// archiveRecording moves one recording into the archive. false with no
// error is a session another replica moved first.
func (a *API) archiveRecording(ctx context.Context, c archiveCandidate) (bool, error) {
	store := storage.ForTenant(a.recordings, tenantIDOf(ctx))
	if !store.Owns(c.Key) {
		return false, fmt.Errorf("recording %q is not this organization's", c.Key)
	}
	obj, err := store.Get(ctx, c.Key)
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	stored := &countingReader{r: obj}
	plain, err := recorder.Open(stored)
	if err != nil {
		_ = obj.Close()
		return false, err
	}

	// The temporary file is named for the format so the archive key says
	// what it holds: <id>.cast.zst, <id>.argusrdp.zst, <id>.argussql.zst.
	tmp, err := os.CreateTemp("", "argus-archive-*"+archiveExt(c.Key))
	if err != nil {
		_ = plain.Close()
		_ = obj.Close()
		return false, err
	}
	defer os.Remove(tmp.Name())
	err = recorder.Compress(tmp, plain, recorder.Archive)
	_ = plain.Close()
	_ = obj.Close()
	if serr := tmp.Sync(); err == nil {
		err = serr
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, fmt.Errorf("recompress: %w", err)
	}

	// The copy that will be kept is the one checked.
	ok, err := verifyStoredFile(tmp.Name(), c.ChainHead)
	if err != nil {
		return false, fmt.Errorf("verify: %w", err)
	}
	if !ok {
		a.reportUnarchivable(ctx, c)
		return false, errArchiveUnverified
	}
	before, after := stored.n, fileSize(tmp.Name())

	key := store.ArchiveKey(c.StartedAt, c.ID, archiveExt(c.Key))
	if _, err := store.PutArchive(ctx, tmp.Name(), key, c.ID, c.ChainHead, c.StartedAt,
		storage.PutOptions{StorageClass: a.ArchiveStorageClass}); err != nil {
		return false, fmt.Errorf("store archive: %w", err)
	}
	moved, err := a.store.MoveRecordingKey(ctx, c.ID, c.Key, key)
	if err != nil {
		return false, fmt.Errorf("move session: %w", err)
	}
	if !moved {
		// Another replica moved it first; what it stored at this key is the
		// same recording, and the old object is its to delete.
		return false, nil
	}
	if err := store.Delete(ctx, c.Key); err != nil {
		// The session names the archive; the old object is an orphan, not
		// a loss. Said, so it can be removed by hand.
		a.log.Warn("archived a recording but could not delete the old copy",
			"session", c.ID, "key", c.Key, "error", err)
	}
	detail := fmt.Sprintf("Recording of session %s moved to the archive at %s, "+
		"recompressed and verified against its chain head %s before the move", c.ID, key, short(c.ChainHead))
	if before > 0 && after > 0 {
		detail += fmt.Sprintf("; %s stored as %s", humanBytes(before), humanBytes(after))
	}
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action:     "recording.archived",
		Severity:   "info",
		ActorEmail: "system:archive",
		Target:     c.ID,
		Detail:     detail + ".",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	return true, nil
}

// reportUnarchivable puts a stored recording that does not verify in the
// audit chain, once: archiving it would put an altered file somewhere new and
// make it look looked after.
func (a *API) reportUnarchivable(ctx context.Context, c archiveCandidate) {
	if n, err := a.store.AuditCountFor(ctx, "recording.unarchivable", c.ID); err == nil && n > 0 {
		return
	}
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action:     "recording.unarchivable",
		Severity:   "critical",
		ActorEmail: "system:archive",
		Target:     c.ID,
		Detail: "The stored recording of session " + c.ID + " on " + c.Host + " (" + c.Key +
			") does not verify against its chain head, so it was not archived. It has " +
			"changed since it was stored, and is left exactly as found.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
}

// verifyStoredFile verifies a stored recording file, of either chain.
func verifyStoredFile(path, head string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	plain, err := recorder.Open(f)
	if err != nil {
		return false, err
	}
	defer plain.Close()
	br := bufio.NewReader(plain)
	if peek, _ := br.Peek(16); tunnel.IsPGRecording(peek) {
		v, err := tunnel.VerifyPGRecording(br, head)
		return err == nil && v.OK, err
	}
	v, err := recorder.Verify(br, head)
	return err == nil && v.OK, err
}

// archiveExt is the extension an archived copy of the object at key has:
// the format's own, then .zst.
func archiveExt(key string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(key, recorder.CompressedExt), ".gz")
	ext := ".cast"
	if i := strings.LastIndexByte(base, '.'); i >= 0 && !strings.Contains(base[i:], "/") {
		ext = base[i:]
	}
	return ext + recorder.CompressedExt
}

func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// countingReader counts what is read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// RecordingsToArchive lists stored recordings that started before cutoff and
// are not archived yet, oldest first.
func (s *Store) RecordingsToArchive(ctx context.Context, cutoff time.Time, limit int) ([]archiveCandidate, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT id::text, recording_key, chain_head, started_at, asset_hostname
		  FROM sessions
		 WHERE recording_key IS NOT NULL AND recording_key <> ''
		   AND chain_head IS NOT NULL AND chain_head <> ''
		   AND state <> 'active'
		   AND started_at < $1
		   AND recording_key NOT LIKE 'archive/%'
		   AND recording_key NOT LIKE '%/archive/%'
		 ORDER BY started_at
		 LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("recordings to archive: %w", err)
	}
	defer rows.Close()
	var out []archiveCandidate
	for rows.Next() {
		var c archiveCandidate
		if err := rows.Scan(&c.ID, &c.Key, &c.ChainHead, &c.StartedAt, &c.Host); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MoveRecordingKey points a session at its archived recording, only if it
// still names from. false means it no longer does.
func (s *Store) MoveRecordingKey(ctx context.Context, id, from, to string) (bool, error) {
	tag, err := s.db(ctx).Exec(ctx, `
		UPDATE sessions SET recording_key = $3
		 WHERE id = $1::uuid AND recording_key = $2`, id, from, to)
	if err != nil {
		return false, fmt.Errorf("move recording key: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// AuditCountFor counts audit entries of one action about one target.
func (s *Store) AuditCountFor(ctx context.Context, action, target string) (int, error) {
	var n int
	err := s.db(ctx).QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE action = $1 AND target = $2`, action, target).Scan(&n)
	return n, err
}
