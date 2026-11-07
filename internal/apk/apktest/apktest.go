// Package apktest builds Android packages for tests, byte for byte the way
// aapt2 and apksigner lay one out: the compiled manifest, then a v2 signing
// block before the central directory, or a v1 signature file.
package apktest

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"math/big"
	"testing"
	"time"
	"unicode/utf16"
)

// What the builder writes; the same values internal/apk reads.
const (
	chunkStringPool = 0x0001
	chunkXML        = 0x0003
	chunkResMap     = 0x0180
	chunkStartElem  = 0x0102
	attrLabel       = 0x01010001
	attrMinSDK      = 0x0101020c
	attrVersionCode = 0x0101021b
	attrVersionName = 0x0101021c
	typeString      = 0x03
	typeIntDec      = 0x10
	// IDV2 and IDV3 are the v2 and v3 signature block ids.
	IDV2 = 0x7109871a
	IDV3 = 0xf05368c0
)

var sigMagic = []byte("APK Sig Block 42")

type xattr struct {
	name  uint32
	typ   uint8
	data  uint32
	raw   uint32
	resID bool
}

// Manifest compiles <manifest package versionCode versionName><uses-sdk
// minSdkVersion/><application label/></manifest>.
func Manifest(pkg string, code uint32, name, label string) []byte {
	strs := []string{"versionCode", "versionName", "minSdkVersion", "label", "package", "manifest", "uses-sdk",
		"application", pkg, name, label}
	var pool bytes.Buffer
	var offs []uint32
	for _, s := range strs {
		offs = append(offs, uint32(pool.Len()))
		u := utf16.Encode([]rune(s))
		_ = binary.Write(&pool, binary.LittleEndian, uint16(len(u)))
		_ = binary.Write(&pool, binary.LittleEndian, u)
		_ = binary.Write(&pool, binary.LittleEndian, uint16(0))
	}
	for pool.Len()%4 != 0 {
		pool.WriteByte(0)
	}
	hs := 28
	start := hs + 4*len(strs)
	var sp bytes.Buffer
	w := func(b *bytes.Buffer, v any) { _ = binary.Write(b, binary.LittleEndian, v) }
	w(&sp, uint16(chunkStringPool))
	w(&sp, uint16(hs))
	w(&sp, uint32(start+pool.Len()))
	w(&sp, uint32(len(strs)))
	w(&sp, uint32(0))
	w(&sp, uint32(0)) // UTF-16
	w(&sp, uint32(start))
	w(&sp, uint32(0))
	for _, o := range offs {
		w(&sp, o)
	}
	sp.Write(pool.Bytes())

	var rm bytes.Buffer
	ids := []uint32{attrVersionCode, attrVersionName, attrMinSDK, attrLabel}
	w(&rm, uint16(chunkResMap))
	w(&rm, uint16(8))
	w(&rm, uint32(8+4*len(ids)))
	for _, id := range ids {
		w(&rm, id)
	}

	elem := func(name uint32, attrs []xattr) []byte {
		var e bytes.Buffer
		size := 16 + 20 + 20*len(attrs)
		w(&e, uint16(chunkStartElem))
		w(&e, uint16(16))
		w(&e, uint32(size))
		w(&e, uint32(1))
		w(&e, uint32(0xFFFFFFFF))
		w(&e, uint32(0xFFFFFFFF))
		w(&e, name)
		w(&e, uint16(20))
		w(&e, uint16(20))
		w(&e, uint16(len(attrs)))
		w(&e, uint16(0))
		w(&e, uint16(0))
		w(&e, uint16(0))
		for _, a := range attrs {
			w(&e, uint32(0xFFFFFFFF))
			w(&e, a.name)
			w(&e, a.raw)
			w(&e, uint16(8))
			e.WriteByte(0)
			e.WriteByte(a.typ)
			w(&e, a.data)
		}
		return e.Bytes()
	}
	none := uint32(0xFFFFFFFF)
	body := append(append(append([]byte{}, sp.Bytes()...), rm.Bytes()...),
		elem(5, []xattr{
			{name: 4, typ: typeString, data: 8, raw: 8},
			{name: 0, typ: typeIntDec, data: code, raw: none},
			{name: 1, typ: typeString, data: 9, raw: 9},
		})...)
	body = append(body, elem(6, []xattr{{name: 2, typ: typeIntDec, data: 29, raw: none}})...)
	body = append(body, elem(7, []xattr{{name: 3, typ: typeString, data: 10, raw: 10}})...)
	var out bytes.Buffer
	w(&out, uint16(chunkXML))
	w(&out, uint16(8))
	w(&out, uint32(8+len(body)))
	out.Write(body)
	return out.Bytes()
}

func Cert(t testing.TB) []byte {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Field"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func Zip(t testing.TB, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, n := range []string{"AndroidManifest.xml", "classes.dex", "META-INF/CERT.RSA"} {
		if body, ok := files[n]; ok {
			f, _ := z.Create(n)
			_, _ = f.Write(body)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func lpOf(parts ...[]byte) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(p)))
		b.Write(p)
	}
	return b.Bytes()
}

// WithV2 puts a signing block carrying der before the central directory.
func WithV2(t testing.TB, z, der []byte, id uint32) []byte {
	t.Helper()
	eocd := bytes.LastIndex(z, []byte{0x50, 0x4b, 0x05, 0x06})
	cd := binary.LittleEndian.Uint32(z[eocd+16:])
	signed := append(lpOf(nil), lpOf(lpOf(der))...) // digests, certificates
	value := lpOf(lpOf(lpOf(signed)))
	var pair bytes.Buffer
	_ = binary.Write(&pair, binary.LittleEndian, uint64(4+len(value)))
	_ = binary.Write(&pair, binary.LittleEndian, id)
	pair.Write(value)
	size := uint64(pair.Len() + 8 + 16)
	var block bytes.Buffer
	_ = binary.Write(&block, binary.LittleEndian, size)
	block.Write(pair.Bytes())
	_ = binary.Write(&block, binary.LittleEndian, size)
	block.Write(sigMagic)
	out := append(append(append([]byte{}, z[:cd]...), block.Bytes()...), z[cd:]...)
	binary.LittleEndian.PutUint32(out[eocd+block.Len()+16:], cd+uint32(block.Len()))
	return out
}

func PKCS7(t testing.TB, der []byte) []byte {
	t.Helper()
	sd, err := asn1.Marshal(struct {
		Version      int
		Digests      asn1.RawValue
		Content      asn1.RawValue
		Certificates asn1.RawValue `asn1:"tag:0"`
	}{
		Version:      1,
		Digests:      asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true},
		Content:      asn1.RawValue{FullBytes: mustMarshal(t, struct{ T asn1.ObjectIdentifier }{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}})},
		Certificates: asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: der},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A RawValue is written as it is, so the [0] EXPLICIT wrapper is spelt
	// out rather than asked of a struct tag.
	return mustMarshal(t, struct {
		Type    asn1.ObjectIdentifier
		Content asn1.RawValue
	}{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2},
		asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: sd}})
}

func mustMarshal(t testing.TB, v any) []byte {
	t.Helper()
	b, err := asn1.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func SHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Signed is a v2-signed package of pkg at code/name, and its certificate.
func Signed(t testing.TB, pkg string, code uint32, name, label string) (apk, cert []byte) {
	t.Helper()
	cert = Cert(t)
	z := Zip(t, map[string][]byte{"AndroidManifest.xml": Manifest(pkg, code, name, label), "classes.dex": []byte("dex\n035")})
	return WithV2(t, z, cert, IDV2), cert
}
