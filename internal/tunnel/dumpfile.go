package tunnel

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

/*
The .argusdump file: an encrypted database archive.

	ARGUSDUMP1\n
	<uint32 header length><header JSON>
	<frame>...

Every backup is encrypted on the server before it is written anywhere, to
the organization's backup key, which is an X25519 key pair derived from the
control plane's root secret. The server can encrypt and cannot decrypt: a
taken disk or bucket yields ciphertext, and the one way to open a backup is a
restore grant that carries that backup's data key for sixty seconds.

A frame is <uint32 plaintext length><ciphertext>, XChaCha20-Poly1305, one
random data key per backup, the nonce the frame's number (so a reordered,
dropped or repeated frame fails), the header as associated data (so a header
from another backup fails), and the final frame flagged in its nonce (so a
truncated file fails rather than restoring a prefix). Header fields are
plain JSON so argus-verify can say what a file is before it has the key.
*/

const (
	dumpMagic = "ARGUSDUMP1\n"
	// DumpFrameSize is the plaintext per frame.
	DumpFrameSize = 1 << 20
	// dumpMaxHeader bounds what a reader will take as a header.
	dumpMaxHeader = 64 << 10
	backupKeyInfo = "argus backup v1:"
	wrapInfo      = "argus backup wrap v1"
	frameInfo     = "argus backup frame v1"
)

// DumpHeader is the plain part of an archive. It is authenticated by every
// frame, so it cannot be moved to another file.
type DumpHeader struct {
	Version       int       `json:"v"`
	BackupID      string    `json:"backupId"`
	DatabaseID    string    `json:"databaseId"`
	Organization  string    `json:"org"`
	DBName        string    `json:"dbname"`
	Host          string    `json:"host"`
	ServerVersion string    `json:"serverVersion,omitempty"`
	DumpVersion   string    `json:"dumpVersion,omitempty"`
	Format        string    `json:"format"`
	Compression   string    `json:"compression,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	Reason        string    `json:"reason,omitempty"`
	// KeyID is the fingerprint of the organization key the data key is
	// wrapped to; WrappedKey and Ephemeral are the wrap.
	KeyID      string `json:"keyId"`
	WrappedKey string `json:"wrappedKey"`
	Ephemeral  string `json:"ephemeral"`
	FrameSize  int    `json:"frameSize"`
}

// DeriveBackupKey is the organization's backup key pair, from the control
// plane's root secret and the organization's id. Nothing is stored; it
// rotates with the secret.
func DeriveBackupKey(secret, orgID string) (*ecdh.PrivateKey, error) {
	if len(secret) < 32 {
		return nil, errors.New("tunnel: signing secret is too short to derive a backup key")
	}
	seed, err := hkdf.Key(sha256.New, []byte(secret), nil, backupKeyInfo+orgID, 32)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(seed)
}

// EncodeBackupPublicKey and DecodeBackupPublicKey carry the public half.
func EncodeBackupPublicKey(k *ecdh.PublicKey) string {
	return base64.StdEncoding.EncodeToString(k.Bytes())
}

func DecodeBackupPublicKey(s string) (*ecdh.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, errors.New("not a backup key")
	}
	return ecdh.X25519().NewPublicKey(b)
}

// BackupKeyID is the fingerprint the console shows and the header records.
func BackupKeyID(k *ecdh.PublicKey) string {
	h := sha256.Sum256(k.Bytes())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])[:16]
}

// wrapKey seals data with an ephemeral X25519 exchange against pub.
func wrapKey(pub *ecdh.PublicKey, data []byte) (wrapped, ephemeral []byte, err error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return nil, nil, err
	}
	salt := append(append([]byte{}, eph.PublicKey().Bytes()...), pub.Bytes()...)
	wk, err := hkdf.Key(sha256.New, shared, salt, wrapInfo, chacha20poly1305.KeySize)
	if err != nil {
		return nil, nil, err
	}
	aead, err := chacha20poly1305.NewX(wk)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize()) // one key per wrap: a fixed nonce is fine
	return aead.Seal(nil, nonce, data, nil), eph.PublicKey().Bytes(), nil
}

// UnwrapBackupKey opens a header's wrapped data key with the organization's
// private key. The control plane does this to issue a restore grant;
// argus-verify does it with an exported key.
func UnwrapBackupKey(priv *ecdh.PrivateKey, h DumpHeader) ([]byte, error) {
	if h.KeyID != BackupKeyID(priv.PublicKey()) {
		return nil, fmt.Errorf("this backup is encrypted to key %s, not %s", h.KeyID, BackupKeyID(priv.PublicKey()))
	}
	ephB, err := base64.StdEncoding.DecodeString(h.Ephemeral)
	if err != nil {
		return nil, errors.New("the header's ephemeral key is not readable")
	}
	eph, err := ecdh.X25519().NewPublicKey(ephB)
	if err != nil {
		return nil, err
	}
	wrapped, err := base64.StdEncoding.DecodeString(h.WrappedKey)
	if err != nil {
		return nil, errors.New("the header's wrapped key is not readable")
	}
	shared, err := priv.ECDH(eph)
	if err != nil {
		return nil, err
	}
	salt := append(append([]byte{}, eph.Bytes()...), priv.PublicKey().Bytes()...)
	wk, err := hkdf.Key(sha256.New, shared, salt, wrapInfo, chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(wk)
	if err != nil {
		return nil, err
	}
	key, err := aead.Open(nil, make([]byte, aead.NonceSize()), wrapped, nil)
	if err != nil {
		return nil, errors.New("the wrapped key does not open with this organization's key")
	}
	return key, nil
}

// frameNonce is the frame number, with the top bit of the last byte set on
// the final frame.
func frameNonce(n uint64, last bool) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	binary.BigEndian.PutUint64(nonce[16:], n)
	if last {
		nonce[0] = 0x80
	}
	return nonce
}

// DumpWriter encrypts a plain archive into w. Close writes the final frame;
// without it the file is truncated and will not open.
type DumpWriter struct {
	w    io.Writer
	aead interface {
		Seal(dst, nonce, plaintext, additionalData []byte) []byte
		NonceSize() int
	}
	ad     []byte
	buf    []byte
	n      uint64
	closed bool
	// Written counts ciphertext bytes, including the header.
	Written int64
	key     []byte
}

// DataKey is a copy of the plain data key, for the writer to read its own
// file back before forgetting it. Forget is the caller's job.
func (d *DumpWriter) DataKey() []byte { return append([]byte{}, d.key...) }

// Forget zeroes the key the writer kept.
func (d *DumpWriter) Forget() {
	for i := range d.key {
		d.key[i] = 0
	}
}

// NewDumpWriter starts an archive encrypted to pub. It returns the writer,
// the header it wrote (with the wrap filled in), and the plain data key the
// caller must forget as soon as the file is closed.
func NewDumpWriter(w io.Writer, pub *ecdh.PublicKey, h DumpHeader) (*DumpWriter, DumpHeader, error) {
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, h, err
	}
	wrapped, eph, err := wrapKey(pub, key)
	if err != nil {
		return nil, h, err
	}
	h.Version = 1
	h.KeyID = BackupKeyID(pub)
	h.WrappedKey = base64.StdEncoding.EncodeToString(wrapped)
	h.Ephemeral = base64.StdEncoding.EncodeToString(eph)
	h.FrameSize = DumpFrameSize
	if h.Format == "" {
		h.Format = "custom"
	}
	hb, err := json.Marshal(h)
	if err != nil {
		return nil, h, err
	}
	if len(hb) > dumpMaxHeader {
		return nil, h, errors.New("the archive header is too large")
	}
	dw, err := newDumpWriter(w, key, hb)
	if err != nil {
		return nil, h, err
	}
	return dw, h, nil
}

func newDumpWriter(w io.Writer, key, hb []byte) (*DumpWriter, error) {
	fk, err := hkdf.Key(sha256.New, key, nil, frameInfo, chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(fk)
	if err != nil {
		return nil, err
	}
	dw := &DumpWriter{w: w, aead: aead, ad: hb, buf: make([]byte, 0, DumpFrameSize), key: key}
	var hdr bytes.Buffer
	hdr.WriteString(dumpMagic)
	_ = binary.Write(&hdr, binary.BigEndian, uint32(len(hb)))
	hdr.Write(hb)
	if _, err := w.Write(hdr.Bytes()); err != nil {
		return nil, err
	}
	dw.Written = int64(hdr.Len())
	return dw, nil
}

func (d *DumpWriter) Write(p []byte) (int, error) {
	if d.closed {
		return 0, errors.New("dump: write after close")
	}
	total := len(p)
	for len(p) > 0 {
		room := DumpFrameSize - len(d.buf)
		if room > len(p) {
			room = len(p)
		}
		d.buf = append(d.buf, p[:room]...)
		p = p[room:]
		if len(d.buf) == DumpFrameSize {
			if err := d.flush(false); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

func (d *DumpWriter) flush(last bool) error {
	ct := d.aead.Seal(nil, frameNonce(d.n, last), d.buf, d.ad)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(d.buf)))
	if _, err := d.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := d.w.Write(ct); err != nil {
		return err
	}
	d.Written += int64(4 + len(ct))
	d.n++
	d.buf = d.buf[:0]
	return nil
}

// Close writes the final frame, which may be empty.
func (d *DumpWriter) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	return d.flush(true)
}

// ReadDumpHeader reads the magic and header, leaving r at the first frame.
func ReadDumpHeader(r *bufio.Reader) (DumpHeader, []byte, error) {
	magic := make([]byte, len(dumpMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != dumpMagic {
		return DumpHeader{}, nil, errors.New("not an Argus backup (bad magic)")
	}
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return DumpHeader{}, nil, errors.New("not an Argus backup (no header)")
	}
	if n == 0 || n > dumpMaxHeader {
		return DumpHeader{}, nil, errors.New("not an Argus backup (header size)")
	}
	hb := make([]byte, n)
	if _, err := io.ReadFull(r, hb); err != nil {
		return DumpHeader{}, nil, errors.New("not an Argus backup (short header)")
	}
	var h DumpHeader
	if err := json.Unmarshal(hb, &h); err != nil || h.Version != 1 {
		return DumpHeader{}, nil, errors.New("not an Argus backup (header)")
	}
	return h, hb, nil
}

// DumpReader decrypts an archive. It refuses a frame out of order, a frame
// altered, a header altered, and a file that ends before its last frame.
type DumpReader struct {
	r    *bufio.Reader
	aead interface {
		Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	}
	ad    []byte
	buf   []byte
	plain []byte
	n     uint64
	done  bool
	// Header is what the file said about itself.
	Header DumpHeader
}

// NewDumpReader opens an archive with its plain data key. expect, when not
// empty, is the backup id the key was issued for: a header naming another
// backup is refused before anything is decrypted.
func NewDumpReader(r io.Reader, key []byte, expect string) (*DumpReader, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 256<<10)
	}
	h, hb, err := ReadDumpHeader(br)
	if err != nil {
		return nil, err
	}
	if expect != "" && h.BackupID != expect {
		return nil, fmt.Errorf("this file is backup %s, and the key is for %s", h.BackupID, expect)
	}
	if h.FrameSize <= 0 || h.FrameSize > 16<<20 {
		return nil, errors.New("not an Argus backup (frame size)")
	}
	fk, err := hkdf.Key(sha256.New, key, nil, frameInfo, chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(fk)
	if err != nil {
		return nil, err
	}
	return &DumpReader{r: br, aead: aead, ad: hb, Header: h, buf: make([]byte, 0, h.FrameSize+aead.Overhead())}, nil
}

func (d *DumpReader) Read(p []byte) (int, error) {
	for len(d.plain) == 0 {
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, d.plain)
	d.plain = d.plain[n:]
	return n, nil
}

func (d *DumpReader) next() error {
	var hdr [4]byte
	if _, err := io.ReadFull(d.r, hdr[:]); err != nil {
		return errors.New("the backup ends before its last frame: it is truncated")
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n > d.Header.FrameSize {
		return errors.New("the backup is damaged (frame size)")
	}
	overhead := chacha20poly1305.Overhead
	d.buf = d.buf[:n+overhead]
	if _, err := io.ReadFull(d.r, d.buf); err != nil {
		return errors.New("the backup ends inside a frame: it is truncated")
	}
	// A frame is last when it opens under the last-frame nonce. Try the
	// ordinary nonce first; the last frame is one of thousands.
	pt, err := d.aead.Open(nil, frameNonce(d.n, false), d.buf, d.ad)
	if err != nil {
		pt, err = d.aead.Open(nil, frameNonce(d.n, true), d.buf, d.ad)
		if err != nil {
			return fmt.Errorf("the backup is damaged at frame %d: it was altered, or this is not its key", d.n)
		}
		d.done = true
	}
	d.n++
	d.plain = pt
	return nil
}

// SHA256Hex is the hash the control plane records for a file.
func SHA256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// IsDumpFile says whether the first bytes of a file are an Argus backup's.
func IsDumpFile(peek []byte) bool {
	return len(peek) >= len(dumpMagic) && string(peek[:len(dumpMagic)]) == dumpMagic
}
