package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A small Redis client: RESP2 over a local socket or loopback, one command at a
time or a short pipeline. Written here rather than pulled in because the agent
needs exactly this much and a reply must be bounded as it is read -- a person
can ask for LRANGE 0 -1 on a list of ten million -- which a general client
would happily hold in memory whole.
*/

// respConn is one connection to Redis.
type respConn struct {
	c net.Conn
	r *bufio.Reader
	w *bufio.Writer
}

// respLimit bounds what one reply keeps: elements past items and bytes past
// budget are read off the wire and dropped, and the value says so.
type respLimit struct {
	items  int
	budget int
}

func dialRedis(ctx context.Context, host string, port int) (*respConn, error) {
	var d net.Dialer
	var (
		c   net.Conn
		err error
	)
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if strings.HasPrefix(host, "/") {
		c, err = d.DialContext(dctx, "unix", host)
	} else {
		c, err = d.DialContext(dctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	if err != nil {
		return nil, err
	}
	return &respConn{c: c, r: bufio.NewReaderSize(c, 64<<10), w: bufio.NewWriterSize(c, 16<<10)}, nil
}

func (c *respConn) close() { _ = c.c.Close() }

func (c *respConn) writeCmd(args []string) {
	fmt.Fprintf(c.w, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(c.w, "$%d\r\n", len(a))
		c.w.WriteString(a)
		c.w.WriteString("\r\n")
	}
}

// do sends one command and reads its reply within timeout. A context
// cancelled while waiting closes the wait at once; the connection is then
// in an unknown state and the caller must not use it again.
func (c *respConn) do(ctx context.Context, timeout time.Duration, lim respLimit, args ...string) (tunnel.RedisValue, error) {
	vs, err := c.pipeline(ctx, timeout, lim, [][]string{args})
	if err != nil {
		return tunnel.RedisValue{}, err
	}
	return vs[0], nil
}

// pipeline sends commands together and reads their replies in order.
func (c *respConn) pipeline(ctx context.Context, timeout time.Duration, lim respLimit, cmds [][]string) ([]tunnel.RedisValue, error) {
	_ = c.c.SetDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { _ = c.c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	for _, args := range cmds {
		c.writeCmd(args)
	}
	if err := c.w.Flush(); err != nil {
		return nil, c.ioErr(ctx, err)
	}
	out := make([]tunnel.RedisValue, 0, len(cmds))
	for range cmds {
		budget := lim.budget
		v, err := c.read(lim.items, &budget, 0)
		if err != nil {
			return nil, c.ioErr(ctx, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func (c *respConn) ioErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errCancelled
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errTimedOut
	}
	return err
}

var (
	errCancelled = errors.New("cancelled")
	errTimedOut  = errors.New("timed out")
)

func (c *respConn) line() (string, error) {
	s, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(s) > 64<<10 {
		return "", errors.New("redis: header line too long")
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// read reads one reply. Elements past items and bytes past budget are read
// and dropped. Nesting is bounded, as Redis's own replies are shallow.
func (c *respConn) read(items int, budget *int, depth int) (tunnel.RedisValue, error) {
	if depth > 8 {
		return tunnel.RedisValue{}, errors.New("redis: reply nested too deep")
	}
	l, err := c.line()
	if err != nil {
		return tunnel.RedisValue{}, err
	}
	if l == "" {
		return tunnel.RedisValue{}, errors.New("redis: empty reply line")
	}
	switch l[0] {
	case '+':
		return tunnel.RedisValue{T: "s", V: l[1:]}, nil
	case '-':
		return tunnel.RedisValue{T: "e", V: l[1:]}, nil
	case ':':
		n, err := strconv.ParseInt(l[1:], 10, 64)
		if err != nil {
			return tunnel.RedisValue{}, errors.New("redis: bad integer")
		}
		return tunnel.RedisValue{T: "i", I: n}, nil
	case '$':
		n, err := strconv.Atoi(l[1:])
		if err != nil || n < -1 {
			return tunnel.RedisValue{}, errors.New("redis: bad bulk length")
		}
		if n == -1 {
			return tunnel.RedisValue{T: "n"}, nil
		}
		keep := min(n, max(*budget, 0), tunnel.MaxCell)
		buf := make([]byte, keep)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return tunnel.RedisValue{}, err
		}
		if _, err := c.r.Discard(n - keep + 2); err != nil {
			return tunnel.RedisValue{}, err
		}
		*budget -= keep
		v := tunnel.RedisValue{T: "b", Cut: keep < n}
		if v.Cut {
			buf = trimUTF8Tail(buf)
		}
		v.V, v.Q = tunnel.RedisText(buf)
		return v, nil
	case '*':
		n, err := strconv.Atoi(l[1:])
		if err != nil || n < -1 {
			return tunnel.RedisValue{}, errors.New("redis: bad array length")
		}
		if n == -1 {
			return tunnel.RedisValue{T: "n"}, nil
		}
		v := tunnel.RedisValue{T: "a", A: make([]tunnel.RedisValue, 0, min(n, items, 1024))}
		for i := 0; i < n; i++ {
			e, err := c.read(items, budget, depth+1)
			if err != nil {
				return v, err
			}
			if i < items && *budget > 0 {
				v.A = append(v.A, e)
			} else {
				v.Cut = true
			}
		}
		if v.Cut {
			v.N = int64(n)
		}
		return v, nil
	}
	return tunnel.RedisValue{}, fmt.Errorf("redis: unexpected reply %q", l[:min(len(l), 16)])
}

// trimUTF8Tail drops a partial UTF-8 sequence a cut left at the end, so a
// text value cut short is still text.
func trimUTF8Tail(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-4; i-- {
		c := b[i]
		if c < 0x80 {
			return b
		}
		if c >= 0xC0 {
			need := 2
			switch {
			case c >= 0xF0:
				need = 4
			case c >= 0xE0:
				need = 3
			}
			if len(b)-i >= need {
				return b
			}
			return b[:i]
		}
	}
	return b
}

// readRedisConf reads what the agent needs from a redis.conf: the admin
// password and the unix socket, if any. Nothing else is kept, and neither is
// ever sent off this server. A missing file is not an error: plenty of
// servers have no password on the default user.
func readRedisConf(path string) (password, socket string, err error) {
	if path == "" {
		return "", "", nil
	}
	if !tunnel.ValidRedisConfPath(path) {
		return "", "", fmt.Errorf("%s is not a redis.conf the agent will read", path)
	}
	b, err := readFileLimited(path, 4<<20)
	if err != nil {
		if errors.Is(err, errNotExist) {
			return "", "", nil
		}
		return "", "", err
	}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' {
			continue
		}
		args, err := tunnel.SplitRedisCommand(line)
		if err != nil || len(args) < 2 {
			continue
		}
		switch strings.ToLower(args[0]) {
		case "requirepass":
			password = args[1]
		case "unixsocket":
			socket = args[1]
		}
	}
	return password, socket, nil
}
