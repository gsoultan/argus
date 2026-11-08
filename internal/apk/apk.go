// Package apk reads what an Android package says about itself: its
// application id, version and signing certificate.
//
// Both ends read it. The control plane shows it when someone adds an APK to
// the software library, so the person sees which app and whose signature
// they are about to hand to devices before anything is saved. The Argus app
// reads the same fields from the file it downloaded before it asks Android
// to install it, so a file that is not the app the console named is refused
// on the device too. It parses and never runs anything, and links nothing
// but the standard library.
package apk

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Info is what a package says about itself.
type Info struct {
	// Package is the application id (com.example.app).
	Package string `json:"package"`
	// VersionCode is the number Android compares to decide an update.
	VersionCode int64 `json:"version_code"`
	// VersionName is what people read ("2.4.0").
	VersionName string `json:"version_name,omitempty"`
	// Label is the application's label when the manifest spells it out; a
	// label in the app's resources is not read.
	Label  string `json:"label,omitempty"`
	MinSDK int    `json:"min_sdk,omitempty"`
	// Signer is the SHA-256 of the first signing certificate, lowercase hex:
	// what Android compares when an installed app is updated.
	Signer string `json:"signer"`
	// Scheme is the newest signature scheme found: "v3", "v2" or "v1".
	Scheme string `json:"scheme"`
}

// RePackage is an application id: dot-separated Java identifiers, at least
// two parts. Nothing else is ever put on a command line.
var RePackage = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)

// Errors the caller can tell apart.
var (
	ErrNotAPK   = errors.New("not an Android package: no AndroidManifest.xml in a zip")
	ErrUnsigned = errors.New("the package is not signed; Android installs only signed packages")
)

// Limits on what is read into memory: a manifest is a few kilobytes, a
// certificate a few hundred bytes.
const (
	maxManifest  = 4 << 20
	maxSigBlock  = 16 << 20
	maxV1SigFile = 1 << 20
)

// Read reads a package of size bytes.
func Read(r io.ReaderAt, size int64) (Info, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return Info{}, ErrNotAPK
	}
	var man *zip.File
	for _, f := range zr.File {
		if f.Name == "AndroidManifest.xml" {
			man = f
			break
		}
	}
	if man == nil {
		return Info{}, ErrNotAPK
	}
	raw, err := readEntry(man, maxManifest)
	if err != nil {
		return Info{}, fmt.Errorf("AndroidManifest.xml: %w", err)
	}
	info, err := parseManifest(raw)
	if err != nil {
		return Info{}, fmt.Errorf("AndroidManifest.xml: %w", err)
	}
	if !RePackage.MatchString(info.Package) {
		return Info{}, fmt.Errorf("the manifest names %q, which is not an application id", info.Package)
	}
	info.Signer, info.Scheme, err = signer(r, size, zr)
	if err != nil {
		return Info{}, err
	}
	return info, nil
}

func readEntry(f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) {
		return nil, errors.New("too large")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("too large")
	}
	return b, nil
}

/* ── The binary manifest ─────────────────────────────────────────────────── */

// Chunk types of Android's binary XML.
const (
	chunkStringPool = 0x0001
	chunkXML        = 0x0003
	chunkResMap     = 0x0180
	chunkStartElem  = 0x0102
)

// Attribute resource ids in the android namespace, which is how a compiled
// manifest names them when the string pool leaves the name out.
const (
	attrLabel       = 0x01010001
	attrMinSDK      = 0x0101020c
	attrVersionCode = 0x0101021b
	attrVersionName = 0x0101021c
)

// Typed value kinds.
const (
	typeString = 0x03
	typeIntDec = 0x10
	typeIntHex = 0x11
)

type manifestReader struct {
	b       []byte
	strings []string
	resIDs  []uint32
}

func parseManifest(b []byte) (Info, error) {
	if len(b) < 8 || binary.LittleEndian.Uint16(b) != chunkXML {
		return Info{}, errors.New("not compiled XML")
	}
	m := &manifestReader{b: b}
	var info Info
	seenManifest := false
	off := int(binary.LittleEndian.Uint16(b[2:]))
	for off+8 <= len(b) {
		typ := binary.LittleEndian.Uint16(b[off:])
		size := int(binary.LittleEndian.Uint32(b[off+4:]))
		if size < 8 || off+size > len(b) {
			return Info{}, errors.New("a chunk runs past the end")
		}
		chunk := b[off : off+size]
		switch typ {
		case chunkStringPool:
			s, err := readStringPool(chunk)
			if err != nil {
				return Info{}, err
			}
			m.strings = s
		case chunkResMap:
			hs := int(binary.LittleEndian.Uint16(chunk[2:]))
			for i := hs; i+4 <= len(chunk); i += 4 {
				m.resIDs = append(m.resIDs, binary.LittleEndian.Uint32(chunk[i:]))
			}
		case chunkStartElem:
			name, attrs, err := m.element(chunk)
			if err != nil {
				return Info{}, err
			}
			switch name {
			case "manifest":
				seenManifest = true
				for _, a := range attrs {
					switch {
					case a.name == "package":
						info.Package = a.str
					case a.is(attrVersionCode, "versionCode"):
						info.VersionCode = a.int()
					case a.is(attrVersionName, "versionName"):
						info.VersionName = a.str
					}
				}
			case "uses-sdk":
				for _, a := range attrs {
					if a.is(attrMinSDK, "minSdkVersion") {
						info.MinSDK = int(a.int())
					}
				}
			case "application":
				for _, a := range attrs {
					if a.is(attrLabel, "label") && a.typ == typeString {
						info.Label = a.str
					}
				}
			}
		}
		off += size
	}
	if !seenManifest {
		return Info{}, errors.New("no <manifest> element")
	}
	return info, nil
}

type attr struct {
	name  string
	resID uint32
	typ   uint8
	data  uint32
	str   string
}

func (a attr) is(id uint32, name string) bool { return a.resID == id || a.name == name }

func (a attr) int() int64 {
	switch a.typ {
	case typeIntDec, typeIntHex:
		return int64(a.data)
	case typeString:
		n, _ := strconv.ParseInt(strings.TrimSpace(a.str), 0, 64)
		return n
	}
	return 0
}

func (m *manifestReader) str(i uint32) string {
	if int(i) < len(m.strings) {
		return m.strings[i]
	}
	return ""
}

func (m *manifestReader) element(c []byte) (string, []attr, error) {
	hs := int(binary.LittleEndian.Uint16(c[2:]))
	if hs < 16 || len(c) < hs+20 {
		return "", nil, errors.New("a short element")
	}
	ext := c[hs:]
	name := m.str(binary.LittleEndian.Uint32(ext[4:]))
	start := int(binary.LittleEndian.Uint16(ext[8:]))
	asize := int(binary.LittleEndian.Uint16(ext[10:]))
	count := int(binary.LittleEndian.Uint16(ext[12:]))
	if asize < 20 {
		return "", nil, errors.New("a short attribute")
	}
	var out []attr
	for i := 0; i < count; i++ {
		p := start + i*asize
		if p+20 > len(ext) {
			return "", nil, errors.New("attributes run past their element")
		}
		a := ext[p:]
		ni := binary.LittleEndian.Uint32(a[4:])
		at := attr{name: m.str(ni), typ: a[15], data: binary.LittleEndian.Uint32(a[16:])}
		if int(ni) < len(m.resIDs) {
			at.resID = m.resIDs[ni]
		}
		if raw := binary.LittleEndian.Uint32(a[8:]); raw != 0xFFFFFFFF {
			at.str = m.str(raw)
		} else if at.typ == typeString {
			at.str = m.str(at.data)
		}
		out = append(out, at)
	}
	return name, out, nil
}

func readStringPool(c []byte) ([]string, error) {
	if len(c) < 28 {
		return nil, errors.New("a short string pool")
	}
	count := int(binary.LittleEndian.Uint32(c[8:]))
	flags := binary.LittleEndian.Uint32(c[16:])
	start := int(binary.LittleEndian.Uint32(c[20:]))
	hs := int(binary.LittleEndian.Uint16(c[2:]))
	if count > 1<<20 || hs+count*4 > len(c) || start > len(c) {
		return nil, errors.New("a string pool that does not fit")
	}
	utf8 := flags&0x100 != 0
	out := make([]string, count)
	for i := 0; i < count; i++ {
		p := start + int(binary.LittleEndian.Uint32(c[hs+i*4:]))
		if p >= len(c) {
			return nil, errors.New("a string past its pool")
		}
		var err error
		if utf8 {
			out[i], err = utf8At(c[p:])
		} else {
			out[i], err = utf16At(c[p:])
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func utf8At(b []byte) (string, error) {
	// The length in UTF-16 units, then in bytes, each one or two bytes.
	n := 0
	skip := func() (int, error) {
		if n >= len(b) {
			return 0, errors.New("a short string")
		}
		v := int(b[n])
		n++
		if v&0x80 != 0 {
			if n >= len(b) {
				return 0, errors.New("a short string")
			}
			v = (v&0x7f)<<8 | int(b[n])
			n++
		}
		return v, nil
	}
	if _, err := skip(); err != nil {
		return "", err
	}
	l, err := skip()
	if err != nil {
		return "", err
	}
	if n+l > len(b) {
		return "", errors.New("a string past its pool")
	}
	return string(b[n : n+l]), nil
}

func utf16At(b []byte) (string, error) {
	if len(b) < 2 {
		return "", errors.New("a short string")
	}
	n := 2
	l := int(binary.LittleEndian.Uint16(b))
	if l&0x8000 != 0 {
		if len(b) < 4 {
			return "", errors.New("a short string")
		}
		l = (l&0x7fff)<<16 | int(binary.LittleEndian.Uint16(b[2:]))
		n = 4
	}
	if n+l*2 > len(b) {
		return "", errors.New("a string past its pool")
	}
	u := make([]uint16, l)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[n+i*2:])
	}
	return string(utf16.Decode(u)), nil
}

/* ── The signature ───────────────────────────────────────────────────────── */

// Signature block ids.
const (
	idV2  = 0x7109871a
	idV3  = 0xf05368c0
	idV31 = 0x1b93ad61
)

var sigMagic = []byte("APK Sig Block 42")

// signer is the first certificate's SHA-256, from the newest scheme present.
func signer(r io.ReaderAt, size int64, zr *zip.Reader) (string, string, error) {
	if pairs, err := sigBlock(r, size); err == nil {
		for _, s := range []struct {
			id     uint32
			scheme string
		}{{idV31, "v3"}, {idV3, "v3"}, {idV2, "v2"}} {
			if v, ok := pairs[s.id]; ok {
				if cert, err := firstCertV2(v); err == nil {
					return sum(cert), s.scheme, nil
				}
			}
		}
	}
	for _, f := range zr.File {
		up := strings.ToUpper(f.Name)
		if !strings.HasPrefix(up, "META-INF/") || strings.Count(up, "/") != 1 {
			continue
		}
		if !(strings.HasSuffix(up, ".RSA") || strings.HasSuffix(up, ".DSA") || strings.HasSuffix(up, ".EC")) {
			continue
		}
		raw, err := readEntry(f, maxV1SigFile)
		if err != nil {
			continue
		}
		if cert, err := firstCertPKCS7(raw); err == nil {
			return sum(cert), "v1", nil
		}
	}
	return "", "", ErrUnsigned
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// sigBlock reads the APK Signing Block that sits right before the zip's
// central directory: id-value pairs.
func sigBlock(r io.ReaderAt, size int64) (map[uint32][]byte, error) {
	cd, err := centralDirOffset(r, size)
	if err != nil {
		return nil, err
	}
	if cd < 32 {
		return nil, errors.New("no signing block")
	}
	foot := make([]byte, 24)
	if _, err := r.ReadAt(foot, cd-24); err != nil {
		return nil, err
	}
	if !bytes.Equal(foot[8:], sigMagic) {
		return nil, errors.New("no signing block")
	}
	bsize := int64(binary.LittleEndian.Uint64(foot))
	if bsize < 24 || bsize > maxSigBlock || bsize+8 > cd {
		return nil, errors.New("a signing block of an impossible size")
	}
	block := make([]byte, bsize+8)
	if _, err := r.ReadAt(block, cd-bsize-8); err != nil {
		return nil, err
	}
	if int64(binary.LittleEndian.Uint64(block)) != bsize {
		return nil, errors.New("a signing block whose sizes disagree")
	}
	pairs := block[8 : len(block)-24]
	out := map[uint32][]byte{}
	for len(pairs) > 0 {
		if len(pairs) < 12 {
			return nil, errors.New("a short signing pair")
		}
		l := binary.LittleEndian.Uint64(pairs)
		if l < 4 || l > uint64(len(pairs)-8) {
			return nil, errors.New("a signing pair past its block")
		}
		id := binary.LittleEndian.Uint32(pairs[8:])
		out[id] = pairs[12 : 8+l]
		pairs = pairs[8+l:]
	}
	return out, nil
}

func centralDirOffset(r io.ReaderAt, size int64) (int64, error) {
	const eocd = 22
	if size < eocd {
		return 0, errors.New("too short for a zip")
	}
	n := int64(eocd + 65535)
	if n > size {
		n = size
	}
	tail := make([]byte, n)
	if _, err := r.ReadAt(tail, size-n); err != nil {
		return 0, err
	}
	for i := len(tail) - eocd; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == 0x06054b50 {
			return int64(binary.LittleEndian.Uint32(tail[i+16:])), nil
		}
	}
	return 0, errors.New("no end of central directory")
}

// lp reads a uint32-length-prefixed slice.
func lp(b []byte) (v, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, errors.New("short")
	}
	l := binary.LittleEndian.Uint32(b)
	if uint64(l) > uint64(len(b)-4) {
		return nil, nil, errors.New("past the end")
	}
	return b[4 : 4+l], b[4+l:], nil
}

// firstCertV2 reads signers → signer → signed data → certificates → the
// first certificate, the layout v2 and v3 share.
func firstCertV2(v []byte) ([]byte, error) {
	signers, _, err := lp(v)
	if err != nil {
		return nil, err
	}
	signer, _, err := lp(signers)
	if err != nil {
		return nil, err
	}
	signed, _, err := lp(signer)
	if err != nil {
		return nil, err
	}
	_, rest, err := lp(signed) // digests
	if err != nil {
		return nil, err
	}
	certs, _, err := lp(rest)
	if err != nil {
		return nil, err
	}
	cert, _, err := lp(certs)
	if err != nil {
		return nil, err
	}
	if len(cert) == 0 {
		return nil, errors.New("an empty certificate")
	}
	return cert, nil
}

// firstCertPKCS7 reads the first certificate of a v1 signature file, a
// PKCS#7 SignedData.
func firstCertPKCS7(b []byte) ([]byte, error) {
	var ci struct {
		Type    asn1.ObjectIdentifier
		Content asn1.RawValue `asn1:"explicit,tag:0"`
	}
	if _, err := asn1.Unmarshal(b, &ci); err != nil {
		return nil, err
	}
	var sd struct {
		Version      int
		Digests      asn1.RawValue
		Content      asn1.RawValue
		Certificates asn1.RawValue `asn1:"optional,tag:0"`
	}
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil {
		return nil, err
	}
	if len(sd.Certificates.Bytes) == 0 {
		return nil, errors.New("no certificates")
	}
	var first asn1.RawValue
	if _, err := asn1.Unmarshal(sd.Certificates.Bytes, &first); err != nil {
		return nil, err
	}
	return first.FullBytes, nil
}

// PKCS7Certificates returns every certificate a PKCS#7 SignedData carries,
// in the order it carries them: an Authenticode signature's (a Windows
// installer's) as well as a v1 APK signature's.
func PKCS7Certificates(b []byte) ([][]byte, error) {
	var ci struct {
		Type    asn1.ObjectIdentifier
		Content asn1.RawValue `asn1:"explicit,tag:0"`
	}
	if _, err := asn1.Unmarshal(b, &ci); err != nil {
		return nil, err
	}
	var sd struct {
		Version      int
		Digests      asn1.RawValue
		Content      asn1.RawValue
		Certificates asn1.RawValue `asn1:"optional,tag:0"`
	}
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil {
		return nil, err
	}
	var out [][]byte
	rest := sd.Certificates.Bytes
	for len(rest) > 0 {
		var c asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &c); err != nil {
			return nil, err
		}
		out = append(out, c.FullBytes)
	}
	if len(out) == 0 {
		return nil, errors.New("no certificates")
	}
	return out, nil
}
