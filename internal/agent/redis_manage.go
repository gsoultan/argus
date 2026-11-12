package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Managing a Redis from the console: its health, editing keys with an undo
copy, bulk changes, configuration and the server's ACL users. The shared
rules (levels, approvals, what may change) are in tunnel/redis_manage.go;
this is how the agent carries them out.

Data changes -- edits and bulk -- run as the person's own ACL user, so Redis
still decides what they may touch. Reading the server's state, configuration
and users, and changing configuration and users, use the admin connection:
those are refused here unless the grant's level allows them, and a change
that waits for approval is refused without one, as the control plane does.

Undo copies are DUMPs written to a directory on this server, kept for a day
and never sent anywhere: the same promise as the admin password.
*/

const (
	undoKeep     = 24 * time.Hour
	undoMaxBytes = int64(2 << 30)
	bulkStep     = 500
	bulkPause    = 20 * time.Millisecond
	// dumpBudget is the largest value an undo copy is kept for.
	dumpBudget = 64 << 20
)

func (s *redisSession) manage(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	need := tunnel.RedisOpLevel(req.Op)
	if tunnel.LevelRank(s.db.Level) < tunnel.LevelRank(need) {
		return tunnel.StoreAnswer{Type: "error",
			Error: "this needs " + need + " or above; this session is at " + s.db.Level}, nil
	}
	if a, waits := tunnel.RedisApproval(req); waits && s.db.ChangesNeedApproval && req.Approval == "" {
		why := "on " + s.g.Host + " this change runs once someone else approves it"
		if err := rec.refused(req.ID, a.Text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why, NeedsApproval: &a}, nil
	}
	switch req.Op {
	case tunnel.RedisOpInfo:
		return s.info(ctx)
	case tunnel.RedisOpEdit:
		return s.edit(ctx, req, rec)
	case tunnel.RedisOpCount:
		return s.count(ctx, req)
	case tunnel.RedisOpBulk:
		return s.bulk(ctx, req, rec)
	case tunnel.RedisOpUndoList:
		return s.undoList()
	case tunnel.RedisOpUndo:
		return s.restore(ctx, req, rec)
	case tunnel.RedisOpConfigGet:
		return s.configGet(ctx)
	case tunnel.RedisOpConfigSet:
		return s.configSet(ctx, req, rec)
	case tunnel.RedisOpSave:
		return s.persist(ctx, req, rec)
	case tunnel.RedisOpACLList:
		return s.aclList(ctx)
	case tunnel.RedisOpACLSet:
		return s.aclSet(ctx, req, rec)
	case tunnel.RedisOpACLDel:
		return s.aclDel(ctx, req, rec)
	case tunnel.RedisOpACLDryRun:
		return s.aclDryRun(ctx, req)
	}
	return tunnel.StoreAnswer{Type: "error", Error: "unknown operation " + req.Op}, nil
}

// errAnswer is a refusal that ends nothing.
func errAnswer(format string, a ...any) (tunnel.StoreAnswer, error) {
	return tunnel.StoreAnswer{Type: "error", Error: fmt.Sprintf(format, a...)}, nil
}

// admin opens the admin connection on this entry's db.
func (s *redisSession) admin(ctx context.Context) (*respConn, error) {
	c, err := s.adminConn(ctx)
	if err != nil {
		return nil, err
	}
	if s.index != 0 {
		v, err := c.do(ctx, 10*time.Second, respLimit{items: 1, budget: 64}, "SELECT", strconv.Itoa(s.index))
		if err == nil && v.T == "e" {
			err = errors.New(v.V)
		}
		if err != nil {
			c.close()
			return nil, err
		}
	}
	return c, nil
}

var adminLim = respLimit{items: 100_000, budget: 16 << 20}

func adminDo(ctx context.Context, c *respConn, args ...string) (tunnel.RedisValue, error) {
	v, err := c.do(ctx, 30*time.Second, adminLim, args...)
	if err == nil && v.T == "e" {
		err = errors.New(v.V)
	}
	return v, err
}

// ---------------------------------------------------------------- health

// RedisInfo is the overview: INFO's fields, the slow log, and what the
// overview warns about that INFO does not say.
type RedisInfo struct {
	Info    map[string]string `json:"info"`
	Slowlog []RedisSlow       `json:"slowlog"`
	// OpenDefault says the default user is on with no password.
	OpenDefault bool `json:"openDefault"`
	// Drift names settings whose running value a restart would undo.
	Drift []string `json:"drift"`
}

// RedisSlow is one slow log entry.
type RedisSlow struct {
	ID      int64    `json:"id"`
	At      int64    `json:"at"`
	Micros  int64    `json:"micros"`
	Command []string `json:"command"`
	Client  string   `json:"client,omitempty"`
	Name    string   `json:"name,omitempty"`
}

func (s *redisSession) info(ctx context.Context) (tunnel.StoreAnswer, error) {
	c, err := s.admin(ctx)
	if err != nil {
		return errAnswer("%s", err)
	}
	defer c.close()
	v, err := adminDo(ctx, c, "INFO", "everything")
	if err != nil {
		if v, err = adminDo(ctx, c, "INFO"); err != nil {
			return errAnswer("Redis did not answer INFO: %s", err)
		}
	}
	out := RedisInfo{Info: parseInfo(v.V), Slowlog: []RedisSlow{}, Drift: []string{}}
	if sl, err := adminDo(ctx, c, "SLOWLOG", "GET", "20"); err == nil {
		out.Slowlog = parseSlowlog(sl)
	}
	if u, err := adminDo(ctx, c, "ACL", "GETUSER", "default"); err == nil {
		f := aclField(u, "flags")
		on, nopass := false, false
		for _, x := range f.A {
			on = on || x.V == "on"
			nopass = nopass || x.V == "nopass"
		}
		out.OpenDefault = on && nopass
	}
	if settings, _, err := s.settings(ctx, c); err == nil {
		for _, st := range settings {
			if st.Drift {
				out.Drift = append(out.Drift, st.Name)
			}
		}
	}
	return tunnel.StoreAnswer{Type: "result", Data: out}, nil
}

func parseInfo(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = v
		}
	}
	return out
}

func parseSlowlog(v tunnel.RedisValue) []RedisSlow {
	out := []RedisSlow{}
	for _, e := range v.A {
		if e.T != "a" || len(e.A) < 4 {
			continue
		}
		sl := RedisSlow{ID: e.A[0].I, At: e.A[1].I, Micros: e.A[2].I, Command: []string{}}
		for _, a := range e.A[3].A {
			sl.Command = append(sl.Command, a.V)
		}
		if len(e.A) >= 6 {
			sl.Client, sl.Name = e.A[4].V, e.A[5].V
		}
		out = append(out, sl)
	}
	return out
}

// aclField is one field of an ACL GETUSER answer.
func aclField(v tunnel.RedisValue, name string) tunnel.RedisValue {
	for i := 0; i+1 < len(v.A); i += 2 {
		if v.A[i].V == name {
			return v.A[i+1]
		}
	}
	return tunnel.RedisValue{}
}

// ---------------------------------------------------------------- edits

// RedisEditResult is how an edit went.
type RedisEditResult struct {
	Ran    int    `json:"ran"`
	UndoID string `json:"undoId,omitempty"`
	// NoUndo says why no undo copy was kept, when one was asked for.
	NoUndo string `json:"noUndo,omitempty"`
}

func (s *redisSession) rawKey(req tunnel.StoreRequest) string {
	if req.KeyQuoted {
		return redisRaw(tunnel.RedisValue{V: req.Key, Q: true})
	}
	return req.Key
}

// edit runs a few commands on one key, each recorded, after keeping an undo
// copy of the key as it was.
func (s *redisSession) edit(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	key := s.rawKey(req)
	if key == "" || len(key) > tunnel.MaxCommand {
		return errAnswer("no key to change")
	}
	if len(req.Edits) == 0 || len(req.Edits) > 200 {
		return errAnswer("an edit is 1 to 200 commands")
	}
	for _, e := range req.Edits {
		if !tunnel.RedisEditable(e) || e[1] != key {
			return errAnswer("%s is not a change to this key the editor makes; use the console", tunnel.JoinRedisCommand(e))
		}
	}
	res := RedisEditResult{}
	if req.Undo {
		res.UndoID, res.NoUndo = s.keepCopy(ctx, []string{key}, "EDIT "+tunnel.JoinRedisCommand([]string{key}))
	}
	for _, e := range req.Edits {
		text := tunnel.JoinRedisCommand(e)
		if err := rec.stmt(req.ID, text); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		start := time.Now()
		v, why, fatal := s.exec(ctx, s.lim(), e...)
		ms := time.Since(start).Milliseconds()
		if why == nil && v.T == "e" {
			why = errors.New(v.V)
		}
		if why != nil {
			if err := rec.fail(req.ID, why.Error(), ms); err != nil {
				return tunnel.StoreAnswer{}, err
			}
			if fatal != nil {
				return tunnel.StoreAnswer{}, fatal
			}
			msg := why.Error()
			if res.Ran > 0 {
				msg = fmt.Sprintf("%s (after %d of %d commands ran)", msg, res.Ran, len(req.Edits))
			}
			return tunnel.StoreAnswer{Type: "error", Error: msg, Data: res}, nil
		}
		if err := rec.done(req.ID, redisTag(v), redisItems(v), ms, v, false); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		res.Ran++
	}
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}

// ---------------------------------------------------------------- bulk

// RedisCount is what a bulk change would touch.
type RedisCount struct {
	Count  int64             `json:"count"`
	Bytes  int64             `json:"bytes"`
	Types  map[string]int64  `json:"types"`
	Sample []tunnel.RedisKey `json:"sample"`
	// Digest is the SHA-256 of the sorted key names, one a line.
	Digest string `json:"digest"`
	// More says more keys match than were counted.
	More bool `json:"more,omitempty"`
}

func validFilter(f tunnel.RedisFilter) string {
	p := strings.TrimSpace(f.Pattern)
	if p == "" || p == "*" && f.Type == "" && f.IdleDays == 0 && !f.NoExpiry {
		return "name a pattern narrower than every key; FLUSHDB is in the console for that"
	}
	if len(p) > 1024 {
		return "the pattern is too long"
	}
	switch f.Type {
	case "", "string", "hash", "list", "set", "zset", "stream":
	default:
		return "unknown key type " + f.Type
	}
	if f.IdleDays < 0 || f.IdleDays > 3650 {
		return "idle days is 0 to 3650"
	}
	return ""
}

// collect walks the db with SCAN for the keys a filter picks, up to limit
// (and one more, to tell there are more).
func (s *redisSession) collect(ctx context.Context, f tunnel.RedisFilter, limit int) ([]string, map[string]int64, error) {
	keys := []string{}
	types := map[string]int64{}
	cursor := "0"
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		args := []string{"SCAN", cursor, "COUNT", "1000", "MATCH", f.Pattern}
		if f.Type != "" {
			args = append(args, "TYPE", f.Type)
		}
		v, why, fatal := s.exec(ctx, respLimit{items: 20000, budget: 16 << 20}, args...)
		if fatal != nil {
			return nil, nil, fatal
		}
		if why == nil && v.T == "e" {
			why = errors.New(v.V)
		}
		if why != nil {
			return nil, nil, why
		}
		if v.T != "a" || len(v.A) != 2 {
			return nil, nil, errors.New("Redis answered SCAN in a way the agent does not know")
		}
		names := make([]string, 0, len(v.A[1].A))
		for _, k := range v.A[1].A {
			names = append(names, redisRaw(k))
		}
		if len(names) > 0 {
			cmds := make([][]string, 0, 3*len(names))
			for _, k := range names {
				cmds = append(cmds, []string{"TYPE", k}, []string{"TTL", k}, []string{"OBJECT", "IDLETIME", k})
			}
			vs, err := s.conn.pipeline(ctx, s.timeout, respLimit{items: 1, budget: 1 << 12}, cmds)
			if err != nil {
				if cerr := s.connect(context.WithoutCancel(ctx)); cerr != nil {
					return nil, nil, fmt.Errorf("the connection to Redis ended: %w", cerr)
				}
				return nil, nil, err
			}
			for i, k := range names {
				typ, ttl, idle := vs[3*i], vs[3*i+1], vs[3*i+2]
				if typ.V == "none" {
					continue
				}
				if f.NoExpiry && ttl.I != -1 {
					continue
				}
				if f.IdleDays > 0 && (idle.T != "i" || idle.I < int64(f.IdleDays)*86400) {
					continue
				}
				keys = append(keys, k)
				types[typ.V]++
				if len(keys) > limit {
					return keys, types, nil
				}
			}
		}
		cursor = v.A[0].V
		if cursor == "0" {
			return keys, types, nil
		}
	}
}

func keysDigest(keys []string) string {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, k := range sorted {
		h.Write([]byte(k))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// count is a dry run: nothing changes, and it is not recorded, as reading
// key names in the browser is not.
func (s *redisSession) count(ctx context.Context, req tunnel.StoreRequest) (tunnel.StoreAnswer, error) {
	if why := validFilter(req.Filter); why != "" {
		return errAnswer("%s", why)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	keys, types, err := s.collect(cctx, req.Filter, tunnel.MaxRedisBulkKeys)
	if err != nil {
		return errAnswer("could not count: %s", err)
	}
	out := RedisCount{Types: types, Sample: []tunnel.RedisKey{}}
	if len(keys) > tunnel.MaxRedisBulkKeys {
		keys, out.More = keys[:tunnel.MaxRedisBulkKeys], true
	}
	out.Count = int64(len(keys))
	out.Digest = keysDigest(keys)
	sample := keys[:min(len(keys), 200)]
	if len(sample) > 0 {
		cmds := make([][]string, len(sample))
		for i, k := range sample {
			cmds[i] = []string{"MEMORY", "USAGE", k}
		}
		if vs, err := s.conn.pipeline(ctx, s.timeout, respLimit{items: 1, budget: 64}, cmds); err == nil {
			var sum int64
			for _, v := range vs {
				sum += v.I
			}
			out.Bytes = sum * out.Count / int64(len(sample))
		}
	}
	for _, k := range keys[:min(len(keys), 20)] {
		shown, q := tunnel.RedisText([]byte(k))
		out.Sample = append(out.Sample, tunnel.RedisKey{Name: shown, Q: q})
	}
	return tunnel.StoreAnswer{Type: "result", Data: out}, nil
}

// RedisBulkResult is how a bulk change went.
type RedisBulkResult struct {
	Changed int64  `json:"changed"`
	Matched int64  `json:"matched"`
	UndoID  string `json:"undoId,omitempty"`
	Stopped string `json:"stopped,omitempty"`
}

func (s *redisSession) bulk(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if why := validFilter(req.Filter); why != "" {
		return errAnswer("%s", why)
	}
	switch req.Action {
	case "delete", "persist":
	case "expire":
		if req.Seconds <= 0 || req.Seconds > 10*365*86400 {
			return errAnswer("an expiry is 1 second to 10 years")
		}
	default:
		return errAnswer("a bulk change deletes, expires or persists keys")
	}
	if req.Max <= 0 || req.Max > tunnel.MaxRedisBulkKeys {
		return errAnswer("say the most keys it may change, 1 to %d; count first", tunnel.MaxRedisBulkKeys)
	}
	text := tunnel.RedisBulkText(req)
	if err := rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: text, Approval: req.Approval}); err != nil {
		return tunnel.StoreAnswer{}, fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	keys, _, err := s.collect(ctx, req.Filter, int(req.Max))
	res := RedisBulkResult{}
	if err != nil {
		if rerr := rec.fail(req.ID, err.Error(), time.Since(start).Milliseconds()); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return errAnswer("nothing changed: %s", err)
	}
	res.Matched = int64(len(keys))
	if res.Matched > req.Max {
		why := fmt.Sprintf("more than %d keys match now, more than it may change; nothing changed. Count again", req.Max)
		if err := rec.fail(req.ID, why, time.Since(start).Milliseconds()); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why, Data: res}, nil
	}
	var undo *undoWriter
	if req.Undo && len(keys) > 0 {
		if undo, err = s.newUndo(text); err != nil {
			why := "no undo copy could be kept, so nothing changed: " + err.Error()
			if err := rec.fail(req.ID, why, time.Since(start).Milliseconds()); err != nil {
				return tunnel.StoreAnswer{}, err
			}
			return errAnswer("%s", why)
		}
		res.UndoID = undo.id
		defer undo.close()
	}
	for i := 0; i < len(keys); i += bulkStep {
		if ctx.Err() != nil {
			res.Stopped = "stopped by you"
			break
		}
		step := keys[i:min(i+bulkStep, len(keys))]
		if undo != nil {
			if err := s.dumpInto(ctx, undo, step); err != nil {
				res.Stopped = "the undo copy could not be kept: " + err.Error()
				break
			}
		}
		var cmds [][]string
		switch req.Action {
		case "delete":
			cmds = [][]string{append([]string{"UNLINK"}, step...)}
		case "expire":
			for _, k := range step {
				cmds = append(cmds, []string{"EXPIRE", k, strconv.FormatInt(req.Seconds, 10)})
			}
		case "persist":
			for _, k := range step {
				cmds = append(cmds, []string{"PERSIST", k})
			}
		}
		vs, err := s.conn.pipeline(ctx, s.timeout, respLimit{items: 1, budget: 1 << 12}, cmds)
		if err != nil {
			if cerr := s.connect(context.WithoutCancel(ctx)); cerr != nil {
				return tunnel.StoreAnswer{}, fmt.Errorf("the connection to Redis ended: %w", cerr)
			}
			res.Stopped = err.Error()
			break
		}
		bad := ""
		for _, v := range vs {
			switch {
			case v.T == "e":
				bad = v.V
			case req.Action == "delete":
				res.Changed += v.I
			default:
				res.Changed += v.I
			}
		}
		if bad != "" {
			res.Stopped = bad
			break
		}
		if i+bulkStep < len(keys) {
			select {
			case <-ctx.Done():
			case <-time.After(bulkPause):
			}
		}
	}
	ms := time.Since(start).Milliseconds()
	if res.Stopped != "" {
		if err := rec.fail(req.ID, fmt.Sprintf("stopped after %d keys: %s", res.Changed, res.Stopped), ms); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Data: res, MS: ms,
			Error: fmt.Sprintf("stopped after %d of %d keys: %s", res.Changed, res.Matched, res.Stopped)}, nil
	}
	if err := rec.done(req.ID, fmt.Sprintf("%s %d", req.Action, res.Changed), res.Changed, ms, res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: res, MS: ms}, nil
}

// ---------------------------------------------------------------- undo

// undoHead is the first line of an undo file.
type undoHead struct {
	By   string `json:"by"`
	At   int64  `json:"at"`
	Port int    `json:"port"`
	DB   int    `json:"db"`
	What string `json:"what"`
}

type undoEntry struct {
	K string `json:"k"`
	T int64  `json:"t"`
	D string `json:"d"`
}

type undoWriter struct {
	id    string
	path  string
	f     *os.File
	w     *bufio.Writer
	room  int64
	count int
}

var undoIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *redisSession) undoDir() string { return s.f.undoDir }

// pruneUndo removes copies older than a day and says how many bytes are
// left in the rest.
func pruneUndo(dir string) int64 {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || !strings.HasSuffix(e.Name(), ".undo") {
			continue
		}
		if time.Since(info.ModTime()) > undoKeep {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		total += info.Size()
	}
	return total
}

func (s *redisSession) newUndo(what string) (*undoWriter, error) {
	dir := s.undoDir()
	if dir == "" {
		return nil, errors.New("this agent keeps no undo copies")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	used := pruneUndo(dir)
	if used >= undoMaxBytes {
		return nil, errors.New("undo copies on this server already hold 2 GB")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(b)
	path := filepath.Join(dir, id+".undo")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	u := &undoWriter{id: id, path: path, f: f, w: bufio.NewWriter(f), room: undoMaxBytes - used}
	head, _ := json.Marshal(undoHead{By: s.g.Email, At: time.Now().Unix(), Port: s.db.Port, DB: s.index, What: what})
	if _, err := u.w.Write(append(head, '\n')); err != nil {
		u.close()
		return nil, err
	}
	return u, nil
}

func (u *undoWriter) add(key string, ttlMS int64, dump string) error {
	b, _ := json.Marshal(undoEntry{K: base64.StdEncoding.EncodeToString([]byte(key)), T: ttlMS,
		D: base64.StdEncoding.EncodeToString([]byte(dump))})
	if int64(len(b)) >= u.room {
		return errors.New("undo copies would pass 2 GB on this server")
	}
	u.room -= int64(len(b)) + 1
	u.count++
	_, err := u.w.Write(append(b, '\n'))
	return err
}

func (u *undoWriter) close() {
	_ = u.w.Flush()
	_ = u.f.Close()
	if u.count == 0 {
		_ = os.Remove(u.path)
	}
}

// dumpInto keeps keys as they are now; a key that is gone is skipped.
func (s *redisSession) dumpInto(ctx context.Context, u *undoWriter, keys []string) error {
	cmds := make([][]string, 0, 2*len(keys))
	for _, k := range keys {
		cmds = append(cmds, []string{"DUMP", k}, []string{"PTTL", k})
	}
	vs, err := s.conn.pipeline(ctx, s.timeout, respLimit{items: 1, budget: dumpBudget}, cmds)
	if err != nil {
		if cerr := s.connect(context.WithoutCancel(ctx)); cerr != nil {
			return fmt.Errorf("the connection to Redis ended: %w", cerr)
		}
		return err
	}
	for i, k := range keys {
		d, t := vs[2*i], vs[2*i+1]
		if d.T == "e" {
			return errors.New(d.V)
		}
		if d.T == "n" {
			continue
		}
		if d.Cut {
			return fmt.Errorf("%s is larger than the 64 MB an undo copy keeps", k)
		}
		if err := u.add(k, t.I, redisRaw(d)); err != nil {
			return err
		}
	}
	return u.w.Flush()
}

// keepCopy keeps an undo copy of a few keys; it returns the copy's id, or
// why there is none. An edit goes ahead without one, and says so.
func (s *redisSession) keepCopy(ctx context.Context, keys []string, what string) (string, string) {
	u, err := s.newUndo(what)
	if err != nil {
		return "", err.Error()
	}
	defer u.close()
	if err := s.dumpInto(ctx, u, keys); err != nil {
		u.count = 0
		return "", err.Error()
	}
	if u.count == 0 {
		return "", "the key did not exist before"
	}
	return u.id, ""
}

// RedisUndo is one undo copy, as the console lists it.
type RedisUndo struct {
	ID      string `json:"id"`
	What    string `json:"what"`
	At      int64  `json:"at"`
	Keys    int    `json:"keys"`
	Bytes   int64  `json:"bytes"`
	Expires int64  `json:"expires"`
}

func (s *redisSession) readUndo(id string) (undoHead, []undoEntry, error) {
	var h undoHead
	if !undoIDRE.MatchString(id) || s.undoDir() == "" {
		return h, nil, errors.New("no such undo copy")
	}
	b, err := readFileLimited(filepath.Join(s.undoDir(), id+".undo"), undoMaxBytes)
	if err != nil {
		return h, nil, errors.New("no such undo copy; it may have passed its day")
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if json.Unmarshal([]byte(lines[0]), &h) != nil || h.By != s.g.Email || h.Port != s.db.Port {
		return h, nil, errors.New("no such undo copy")
	}
	if time.Since(time.Unix(h.At, 0)) > undoKeep {
		return h, nil, errors.New("that undo copy has passed its day")
	}
	out := make([]undoEntry, 0, len(lines)-1)
	for _, l := range lines[1:] {
		var e undoEntry
		if json.Unmarshal([]byte(l), &e) == nil {
			out = append(out, e)
		}
	}
	return h, out, nil
}

func (s *redisSession) undoList() (tunnel.StoreAnswer, error) {
	out := []RedisUndo{}
	dir := s.undoDir()
	pruneUndo(dir)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".undo")
		if !ok || !undoIDRE.MatchString(id) {
			continue
		}
		h, entries, err := s.readUndo(id)
		if err != nil || h.DB != s.index {
			continue
		}
		info, _ := e.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		out = append(out, RedisUndo{ID: id, What: h.What, At: h.At, Keys: len(entries), Bytes: size,
			Expires: h.At + int64(undoKeep/time.Second)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return tunnel.StoreAnswer{Type: "result", Data: out}, nil
}

// RedisRestore is how putting a copy back went.
type RedisRestore struct {
	Restored int `json:"restored"`
	// Expired are keys whose TTL ran out since; they are not put back.
	Expired int `json:"expired"`
}

// restore puts an undo copy back, over whatever the keys hold now. RESTORE
// is a dangerous command no level below DBA has, so it runs as admin, and
// only for the person who made the copy.
func (s *redisSession) restore(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	h, entries, err := s.readUndo(req.UndoID)
	if err != nil {
		return errAnswer("%s", err)
	}
	if h.DB != s.index {
		return errAnswer("that copy is from db %d; open that db to put it back", h.DB)
	}
	text := "UNDO " + req.UndoID + " (" + h.What + ")"
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	c, err := s.admin(ctx)
	if err != nil {
		_ = rec.fail(req.ID, err.Error(), 0)
		return errAnswer("%s", err)
	}
	defer c.close()
	res := RedisRestore{}
	elapsed := time.Since(time.Unix(h.At, 0)).Milliseconds()
	var cmds [][]string
	flush := func() error {
		if len(cmds) == 0 {
			return nil
		}
		vs, err := c.pipeline(ctx, 30*time.Second, respLimit{items: 1, budget: 1 << 12}, cmds)
		cmds = cmds[:0]
		if err != nil {
			return err
		}
		for _, v := range vs {
			if v.T == "e" {
				return errors.New(v.V)
			}
			res.Restored++
		}
		return nil
	}
	for _, e := range entries {
		k, err1 := base64.StdEncoding.DecodeString(e.K)
		d, err2 := base64.StdEncoding.DecodeString(e.D)
		if err1 != nil || err2 != nil {
			continue
		}
		ttl := int64(0)
		if e.T >= 0 {
			ttl = e.T - elapsed
			if ttl <= 0 {
				res.Expired++
				continue
			}
		}
		cmds = append(cmds, []string{"RESTORE", string(k), strconv.FormatInt(ttl, 10), string(d), "REPLACE"})
		if len(cmds) >= 200 {
			if err := flush(); err != nil {
				return s.restoreFailed(req, rec, res, err, start)
			}
		}
	}
	if err := flush(); err != nil {
		return s.restoreFailed(req, rec, res, err, start)
	}
	_ = os.Remove(filepath.Join(s.undoDir(), req.UndoID+".undo"))
	if err := rec.done(req.ID, fmt.Sprintf("restore %d", res.Restored), int64(res.Restored),
		time.Since(start).Milliseconds(), res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}

func (s *redisSession) restoreFailed(req tunnel.StoreRequest, rec *stmtRecorder, res RedisRestore, err error, start time.Time) (tunnel.StoreAnswer, error) {
	why := fmt.Sprintf("stopped after %d keys: %s", res.Restored, err)
	if rerr := rec.fail(req.ID, why, time.Since(start).Milliseconds()); rerr != nil {
		return tunnel.StoreAnswer{}, rerr
	}
	return tunnel.StoreAnswer{Type: "error", Error: why, Data: res}, nil
}

// ---------------------------------------------------------------- config

// RedisSetting is one setting, as the configuration page shows it.
type RedisSetting struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	File     string `json:"file,omitempty"`
	InFile   bool   `json:"inFile"`
	Default  string `json:"default,omitempty"`
	Group    string `json:"group"`
	Settable bool   `json:"settable"`
	Secret   bool   `json:"secret,omitempty"`
	// Set says a secret has a value.
	Set   bool `json:"set,omitempty"`
	Drift bool `json:"drift,omitempty"`
}

// RedisConfig is the configuration page.
type RedisConfig struct {
	Settings []RedisSetting `json:"settings"`
	File     string         `json:"file"`
	FileRead bool           `json:"fileRead"`
	ACLFile  bool           `json:"aclFile"`
}

// settings reads every setting, and the file's, without secrets.
func (s *redisSession) settings(ctx context.Context, c *respConn) ([]RedisSetting, RedisConfig, error) {
	cfg := RedisConfig{File: s.conf, Settings: []RedisSetting{}}
	v, err := adminDo(ctx, c, "CONFIG", "GET", "*")
	if err != nil {
		return nil, cfg, err
	}
	file := map[string]string{}
	if b, err := readFileLimited(s.conf, 4<<20); err == nil {
		file, cfg.FileRead = tunnel.RedisConfFile(string(b)), true
	}
	for i := 0; i+1 < len(v.A); i += 2 {
		name, val := strings.ToLower(v.A[i].V), v.A[i+1].V
		if name == "aclfile" {
			cfg.ACLFile = val != ""
		}
		st := RedisSetting{Name: name, Value: val, Group: "advanced"}
		if g, ok := tunnel.RedisSettings[name]; ok {
			st.Group, st.Default = g.Group, g.Default
		}
		st.Settable = tunnel.RedisConfigSettable(name)
		if tunnel.RedisConfigLocked(name) {
			st.Group = "locked"
		}
		fv, inFile := file[name]
		st.File, st.InFile = fv, inFile
		if tunnel.RedisConfigSecret(name) {
			st.Secret, st.Set, st.Value, st.File = true, val != "", "", ""
		} else if cfg.FileRead && st.Settable {
			if inFile {
				st.Drift = !tunnel.RedisSameSetting(name, val, fv)
			} else if st.Default != "" {
				st.Drift = !tunnel.RedisSameSetting(name, val, st.Default)
			}
		}
		cfg.Settings = append(cfg.Settings, st)
	}
	sort.Slice(cfg.Settings, func(i, j int) bool { return cfg.Settings[i].Name < cfg.Settings[j].Name })
	return cfg.Settings, cfg, nil
}

func (s *redisSession) configGet(ctx context.Context) (tunnel.StoreAnswer, error) {
	c, err := s.admin(ctx)
	if err != nil {
		return errAnswer("%s", err)
	}
	defer c.close()
	_, cfg, err := s.settings(ctx, c)
	if err != nil {
		return errAnswer("Redis did not answer CONFIG GET: %s", err)
	}
	return tunnel.StoreAnswer{Type: "result", Data: cfg}, nil
}

// RedisConfigResult is how a configuration change went.
type RedisConfigResult struct {
	Before    map[string]string `json:"before"`
	After     map[string]string `json:"after"`
	Rewritten bool              `json:"rewritten"`
	// RewriteError says the running server changed but the file did not.
	RewriteError string `json:"rewriteError,omitempty"`
}

func (s *redisSession) configSet(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if why := tunnel.ValidRedisConfigItems(req.Config); why != "" {
		return errAnswer("%s", why)
	}
	a, _ := tunnel.RedisApproval(req)
	if err := rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: a.Text, Approval: req.Approval}); err != nil {
		return tunnel.StoreAnswer{}, fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	fail := func(why string) (tunnel.StoreAnswer, error) {
		if err := rec.fail(req.ID, why, time.Since(start).Milliseconds()); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return errAnswer("%s", why)
	}
	c, err := s.admin(ctx)
	if err != nil {
		return fail(err.Error())
	}
	defer c.close()
	res := RedisConfigResult{Before: map[string]string{}, After: map[string]string{}}
	read := func(into map[string]string) {
		for _, it := range req.Config {
			if v, err := adminDo(ctx, c, "CONFIG", "GET", it.Name); err == nil && len(v.A) == 2 {
				into[strings.ToLower(it.Name)] = v.A[1].V
			}
		}
	}
	read(res.Before)
	args := []string{"CONFIG", "SET"}
	for _, it := range req.Config {
		args = append(args, strings.ToLower(it.Name), it.Value)
	}
	if _, err := adminDo(ctx, c, args...); err != nil {
		if len(req.Config) > 1 && strings.Contains(strings.ToLower(err.Error()), "wrong number of arguments") {
			// Redis 6 sets one at a time.
			for _, it := range req.Config {
				if _, err := adminDo(ctx, c, "CONFIG", "SET", strings.ToLower(it.Name), it.Value); err != nil {
					read(res.After)
					return fail(fmt.Sprintf("Redis refused %s: %s", it.Name, err))
				}
			}
		} else {
			return fail("Redis refused the change: " + err.Error())
		}
	}
	read(res.After)
	if req.Rewrite {
		if _, err := adminDo(ctx, c, "CONFIG", "REWRITE"); err != nil {
			res.RewriteError = err.Error()
		} else {
			res.Rewritten = true
		}
	}
	if err := rec.done(req.ID, "config set "+strconv.Itoa(len(req.Config)), int64(len(req.Config)),
		time.Since(start).Milliseconds(), res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}

func (s *redisSession) persist(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	var cmd string
	switch req.Name {
	case "bgsave":
		cmd = "BGSAVE"
	case "bgrewriteaof":
		cmd = "BGREWRITEAOF"
	default:
		return errAnswer("save is bgsave or bgrewriteaof")
	}
	if err := rec.stmt(req.ID, cmd); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	c, err := s.admin(ctx)
	if err != nil {
		_ = rec.fail(req.ID, err.Error(), 0)
		return errAnswer("%s", err)
	}
	defer c.close()
	v, err := adminDo(ctx, c, cmd)
	if err != nil {
		if rerr := rec.fail(req.ID, err.Error(), 0); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return errAnswer("%s", err)
	}
	if err := rec.done(req.ID, redisTag(v), 0, 0, v, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Value: &v}, nil
}

// ---------------------------------------------------------------- users

// RedisUser is one ACL user, as the console shows it. Passwords are never
// read; their hashes are shown short (RedisHashRef).
type RedisUser struct {
	Name      string   `json:"name"`
	On        bool     `json:"on"`
	NoPass    bool     `json:"nopass"`
	Passwords []string `json:"passwords"`
	Keys      []string `json:"keys"`
	Channels  []string `json:"channels"`
	Commands  []string `json:"commands"`
	Selectors []string `json:"selectors"`
	Argus     bool     `json:"argus"`
	Self      bool     `json:"self"`
	Clients   int      `json:"clients"`
}

// RedisUsers is the users page.
type RedisUsers struct {
	Users   []RedisUser `json:"users"`
	ACLFile bool        `json:"aclFile"`
}

// parseACLLine reads one ACL LIST line.
func parseACLLine(line string) RedisUser {
	u := RedisUser{Passwords: []string{}, Keys: []string{}, Channels: []string{}, Commands: []string{}, Selectors: []string{}}
	// Selectors are in parentheses and hold spaces; take them out first.
	for {
		i := strings.Index(line, "(")
		if i < 0 {
			break
		}
		j := strings.Index(line[i:], ")")
		if j < 0 {
			break
		}
		u.Selectors = append(u.Selectors, line[i:i+j+1])
		line = line[:i] + line[i+j+1:]
	}
	f := strings.Fields(line)
	if len(f) >= 2 && f[0] == "user" {
		u.Name = f[1]
		f = f[2:]
	}
	for _, t := range f {
		switch {
		case t == "on":
			u.On = true
		case t == "off":
			u.On = false
		case t == "nopass":
			u.NoPass = true
		case strings.HasPrefix(t, "#"):
			u.Passwords = append(u.Passwords, tunnel.RedisHashRef(t[1:]))
		case strings.HasPrefix(t, "~"), strings.HasPrefix(t, "%"):
			u.Keys = append(u.Keys, t)
		case strings.HasPrefix(t, "&"), t == "resetchannels":
			if t != "resetchannels" {
				u.Channels = append(u.Channels, t)
			}
		case strings.HasPrefix(t, "+"), strings.HasPrefix(t, "-"):
			u.Commands = append(u.Commands, t)
		}
	}
	return u
}

func (s *redisSession) aclList(ctx context.Context) (tunnel.StoreAnswer, error) {
	c, err := s.admin(ctx)
	if err != nil {
		return errAnswer("%s", err)
	}
	defer c.close()
	v, err := adminDo(ctx, c, "ACL", "LIST")
	if err != nil {
		return errAnswer("Redis did not answer ACL LIST: %s", err)
	}
	self := ""
	if w, err := adminDo(ctx, c, "ACL", "WHOAMI"); err == nil {
		self = w.V
	}
	clients := map[string]int{}
	if cl, err := adminDo(ctx, c, "CLIENT", "LIST"); err == nil {
		for _, line := range strings.Split(cl.V, "\n") {
			for _, f := range strings.Fields(line) {
				if u, ok := strings.CutPrefix(f, "user="); ok {
					clients[u]++
				}
			}
		}
	}
	out := RedisUsers{Users: []RedisUser{}}
	if a, err := adminDo(ctx, c, "CONFIG", "GET", "aclfile"); err == nil && len(a.A) == 2 {
		out.ACLFile = a.A[1].V != ""
	}
	for _, l := range v.A {
		u := parseACLLine(l.V)
		if u.Name == "" {
			continue
		}
		u.Argus, u.Self, u.Clients = tunnel.IsStoreUser(u.Name), u.Name == self, clients[u.Name]
		out.Users = append(out.Users, u)
	}
	return tunnel.StoreAnswer{Type: "result", Data: out}, nil
}

// RedisUserResult is how a change to a user went.
type RedisUserResult struct {
	// Saved says how the change was kept for a restart: "aclfile",
	// "redis.conf", or "" with SaveError.
	Saved     string `json:"saved"`
	SaveError string `json:"saveError,omitempty"`
}

// guardUser refuses users Argus does not change from here: its own session
// users, and the one the agent signs in as.
func (s *redisSession) guardUser(ctx context.Context, c *respConn, name string) string {
	if !tunnel.ValidRedisUserName(name) {
		return "not a user Argus changes from here: " + name
	}
	if w, err := adminDo(ctx, c, "ACL", "WHOAMI"); err == nil && w.V == name {
		return name + " is the user the agent signs in as; change it on the server itself"
	}
	return ""
}

// saveACL keeps the users for a restart: ACL SAVE with an aclfile,
// otherwise CONFIG REWRITE, which writes them into redis.conf.
func saveACL(ctx context.Context, c *respConn) RedisUserResult {
	if a, err := adminDo(ctx, c, "CONFIG", "GET", "aclfile"); err == nil && len(a.A) == 2 && a.A[1].V != "" {
		if _, err := adminDo(ctx, c, "ACL", "SAVE"); err != nil {
			return RedisUserResult{SaveError: err.Error()}
		}
		return RedisUserResult{Saved: "aclfile"}
	}
	if _, err := adminDo(ctx, c, "CONFIG", "REWRITE"); err != nil {
		return RedisUserResult{SaveError: err.Error()}
	}
	return RedisUserResult{Saved: "redis.conf"}
}

func (s *redisSession) aclSet(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if req.State != "" && req.State != "on" && req.State != "off" {
		return errAnswer("a user is on or off")
	}
	if len(req.AddHashes)+len(req.RemoveHashes) > 8 {
		return errAnswer("change at most 8 passwords at a time")
	}
	for _, h := range req.AddHashes {
		if !tunnel.ValidRedisHash(h) {
			return errAnswer("a new password goes to Redis as its SHA-256, 64 hex characters")
		}
	}
	for _, h := range req.RemoveHashes {
		if !tunnel.ValidRedisHashRef(h) {
			return errAnswer("not a password of this user: %s", h)
		}
	}
	if req.Rules != nil {
		if why := tunnel.ValidRedisRules(req.Rules, ""); why != "" {
			return errAnswer("%s", why)
		}
	}
	if req.Create && req.Rules == nil {
		return errAnswer("a new user needs its rules")
	}
	a, _ := tunnel.RedisApproval(req)
	c, err := s.admin(ctx)
	if err != nil {
		return errAnswer("%s", err)
	}
	defer c.close()
	if why := s.guardUser(ctx, c, req.User); why != "" {
		return errAnswer("%s", why)
	}
	cur, err := adminDo(ctx, c, "ACL", "GETUSER", req.User)
	exists := err == nil && cur.T == "a"
	switch {
	case err != nil:
		return errAnswer("Redis did not answer ACL GETUSER: %s", err)
	case req.Create && exists:
		return errAnswer("%s already exists on this Redis", req.User)
	case !req.Create && !exists:
		return errAnswer("%s is not a user on this Redis", req.User)
	}
	// The short hashes the console shows, back to the hashes Redis has.
	args := tunnel.RedisSetUserArgs(req)
	if len(req.RemoveHashes) > 0 {
		full := map[string]string{}
		for _, p := range aclField(cur, "passwords").A {
			full[tunnel.RedisHashRef(p.V)] = p.V
		}
		for i, x := range args {
			if ref, ok := strings.CutPrefix(x, "!"); ok && tunnel.ValidRedisHashRef(ref) && !tunnel.ValidRedisHash(ref) {
				h, ok := full[ref]
				if !ok {
					return errAnswer("%s has no password %s", req.User, ref)
				}
				args[i] = "!" + h
			}
		}
	}
	if err := rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: a.Text, Approval: req.Approval}); err != nil {
		return tunnel.StoreAnswer{}, fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	if _, err := adminDo(ctx, c, args...); err != nil {
		if rerr := rec.fail(req.ID, err.Error(), time.Since(start).Milliseconds()); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return errAnswer("Redis refused it: %s", err)
	}
	res := saveACL(ctx, c)
	if err := rec.done(req.ID, "acl setuser", 1, time.Since(start).Milliseconds(), res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}

func (s *redisSession) aclDel(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if req.User == "default" {
		return errAnswer("default is never deleted; give it a password or turn it off")
	}
	c, err := s.admin(ctx)
	if err != nil {
		return errAnswer("%s", err)
	}
	defer c.close()
	if why := s.guardUser(ctx, c, req.User); why != "" {
		return errAnswer("%s", why)
	}
	a, _ := tunnel.RedisApproval(req)
	if err := rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: a.Text, Approval: req.Approval}); err != nil {
		return tunnel.StoreAnswer{}, fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	v, err := adminDo(ctx, c, "ACL", "DELUSER", req.User)
	if err == nil && v.I == 0 {
		err = errors.New(req.User + " is not a user on this Redis")
	}
	if err != nil {
		if rerr := rec.fail(req.ID, err.Error(), time.Since(start).Milliseconds()); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return errAnswer("%s", err)
	}
	res := saveACL(ctx, c)
	if err := rec.done(req.ID, "acl deluser", 1, time.Since(start).Milliseconds(), res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}

// RedisDryRun is whether rules allow a command.
type RedisDryRun struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// aclDryRun asks Redis whether rules allow a command, on a user made for
// the question and removed after it. Nothing runs.
func (s *redisSession) aclDryRun(ctx context.Context, req tunnel.StoreRequest) (tunnel.StoreAnswer, error) {
	if why := tunnel.ValidRedisRules(req.Rules, ""); why != "" {
		return errAnswer("%s", why)
	}
	args, err := tunnel.SplitRedisCommand(strings.TrimSpace(req.Command))
	if err != nil || len(args) == 0 {
		return errAnswer("name a command to check")
	}
	c, err := s.admin(ctx)
	if err != nil {
		return errAnswer("%s", err)
	}
	defer c.close()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	tmp := tunnel.StoreUserPrefix + "dryrun-" + hex.EncodeToString(b)
	if _, err := adminDo(ctx, c, append([]string{"ACL", "SETUSER", tmp, "reset", "off"}, req.Rules...)...); err != nil {
		return errAnswer("Redis refused the rules: %s", err)
	}
	defer func() { _, _ = adminDo(context.WithoutCancel(ctx), c, "ACL", "DELUSER", tmp) }()
	v, err := c.do(ctx, 10*time.Second, respLimit{items: 4, budget: 1 << 12}, append([]string{"ACL", "DRYRUN", tmp}, args...)...)
	if err != nil {
		return errAnswer("%s", err)
	}
	if v.T == "e" {
		if strings.Contains(strings.ToLower(v.V), "unknown subcommand") {
			return errAnswer("this Redis has no ACL DRYRUN; it needs Redis 7")
		}
		return errAnswer("%s", v.V)
	}
	if v.V == "OK" {
		return tunnel.StoreAnswer{Type: "result", Data: RedisDryRun{Allowed: true}}, nil
	}
	return tunnel.StoreAnswer{Type: "result", Data: RedisDryRun{Reason: strings.ReplaceAll(v.V, tmp, "this level")}}, nil
}
