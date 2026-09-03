package credssp

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// scriptedServer plays the target's half of the exchange over a real socket.
type scriptedServer struct {
	t *testing.T
	// bindTo is the public key the server binds its answer to. Differing from
	// what the client holds is what an interposed proxy looks like.
	bindTo []byte
	// version the server claims.
	version int
	// sawCredentials records whether the credentials were delegated.
	sawCredentials chan []byte
}

func (s *scriptedServer) run(conn net.Conn, creds Credentials) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// 1. read NEGOTIATE
	if _, err := readRequest(conn); err != nil {
		return
	}

	// 2. send CHALLENGE
	targetInfo := buildTargetInfo(s.t, "CORP", "WIN-01")
	challenge := buildChallenge(s.t, "WIN-01", mustHex(s.t, "0123456789abcdef"),
		NegotiateUnicode|NegotiateNTLM|NegotiateExtendedSession|
			NegotiateKeyExchange|Negotiate128, targetInfo)
	if err := writeRequest(conn, Request{Version: s.version, NegoToken: challenge}); err != nil {
		return
	}

	// 3. read AUTHENTICATE and recover the session key the client chose
	req, err := readRequest(conn)
	if err != nil {
		return
	}
	sessionKey, err := recoverSessionKey(req.NegoToken, creds, mustHex(s.t, "0123456789abcdef"))
	if err != nil {
		s.t.Errorf("server could not recover the session key: %v", err)
		return
	}
	sealer, err := NewSealer(sessionKey, NegotiateKeyExchange, false)
	if err != nil {
		return
	}
	// The client's binding arrives first and advances the receive keystream.
	if _, err := sealer.Unseal(req.PubKeyAuth); err != nil {
		s.t.Errorf("server could not unseal the client binding: %v", err)
		return
	}

	// 4. answer with our own binding
	var answer []byte
	if s.version < 5 {
		key := append([]byte{}, s.bindTo...)
		key[0]++
		answer = sealer.Seal(key)
	} else {
		answer = sealer.Seal(bindingHash(serverToClientBinding, req.ClientNonce, s.bindTo))
	}
	if err := writeRequest(conn, Request{Version: s.version, PubKeyAuth: answer}); err != nil {
		return
	}

	// 5. the credentials, if the client decided to send them
	final, err := readRequest(conn)
	if err != nil {
		s.sawCredentials <- nil
		return
	}
	plain, err := sealer.Unseal(final.AuthInfo)
	if err != nil {
		s.sawCredentials <- nil
		return
	}
	s.sawCredentials <- plain
}

// recoverSessionKey does what a server does: recompute the response key from
// the password it knows and decrypt the client's exported session key.
func recoverSessionKey(authMsg []byte, creds Credentials, serverChallenge []byte) ([]byte, error) {
	// Session key field descriptor sits at offset 52.
	if len(authMsg) < 64 {
		return nil, errors.New("authenticate message too short")
	}
	length := int(authMsg[52]) | int(authMsg[53])<<8
	offset := int(authMsg[56]) | int(authMsg[57])<<8 |
		int(authMsg[58])<<16 | int(authMsg[59])<<24
	if offset+length > len(authMsg) {
		return nil, errors.New("session key field out of range")
	}
	encrypted := authMsg[offset : offset+length]

	// NT response descriptor is at offset 20; the proof string is its first 16
	// bytes, which is what the key exchange key is derived from.
	ntLen := int(authMsg[20]) | int(authMsg[21])<<8
	ntOff := int(authMsg[24]) | int(authMsg[25])<<8 |
		int(authMsg[26])<<16 | int(authMsg[27])<<24
	if ntOff+ntLen > len(authMsg) || ntLen < 16 {
		return nil, errors.New("nt response out of range")
	}
	ntProof := authMsg[ntOff : ntOff+16]

	responseKey := NTOWFv2(creds.User, creds.Password, creds.Domain)
	kek := hmacMD5(responseKey, ntProof)
	return rc4Crypt(kek, encrypted), nil
}

func pair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); done <- c }()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-done
	t.Cleanup(func() { client.Close(); server.Close() })
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	return client, server
}

func TestAuthenticateDelegatesCredentialsWhenTheBindingMatches(t *testing.T) {
	creds := Credentials{Domain: "CORP", User: "ops", Password: "hunter2"}
	pubKey := []byte("the target's public key")

	c, s := pair(t)
	srv := &scriptedServer{t: t, bindTo: pubKey, version: 6,
		sawCredentials: make(chan []byte, 1)}
	go srv.run(s, creds)

	a := &Authenticator{PublicKey: pubKey, Credentials: creds}
	result, err := a.Authenticate(c)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if result.LegacyBinding {
		t.Error("a version 6 exchange used the legacy binding")
	}

	select {
	case got := <-srv.sawCredentials:
		if got == nil {
			t.Fatal("the credentials never arrived")
		}
		if !bytes.Contains(got, utf16le("hunter2")) {
			t.Error("the delegated credentials do not carry the password")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the credentials")
	}
}

// The property the whole step exists for: a target binding to a different
// public key is on a different channel, and the password must not follow.
func TestAuthenticateWithholdsCredentialsOnABindingMismatch(t *testing.T) {
	creds := Credentials{Domain: "CORP", User: "ops", Password: "hunter2"}

	c, s := pair(t)
	srv := &scriptedServer{t: t, bindTo: []byte("a different key"), version: 6,
		sawCredentials: make(chan []byte, 1)}
	go srv.run(s, creds)

	a := &Authenticator{PublicKey: []byte("the real key"), Credentials: creds}
	_, err := a.Authenticate(c)
	if !errors.Is(err, ErrPubKeyMismatch) {
		t.Fatalf("err = %v, want ErrPubKeyMismatch", err)
	}

	select {
	case got := <-srv.sawCredentials:
		if got != nil {
			t.Fatal("the password was delegated to a mismatched channel")
		}
	case <-time.After(2 * time.Second):
		// Nothing arrived, which is the correct outcome.
	}
}

func TestAuthenticateRefusesWithoutAPublicKey(t *testing.T) {
	a := &Authenticator{Credentials: Credentials{User: "ops"}}
	if _, err := a.Authenticate(new(bytes.Buffer)); err == nil {
		t.Error("the exchange ran with nothing to bind to")
	}
}

// The length is read from the wire before anything is authenticated.
func TestReadRequestBoundsTheLength(t *testing.T) {
	// 0x84 = four length bytes, claiming 0x7FFFFFFF.
	r := bytes.NewReader([]byte{0x30, 0x84, 0x7F, 0xFF, 0xFF, 0xFF})
	if _, err := readRequest(r); !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

func TestReadRequestRejectsNonSequences(t *testing.T) {
	if _, err := readRequest(bytes.NewReader([]byte{0x02, 0x01, 0x06})); !errors.Is(err, ErrMalformed) {
		t.Error("a non-SEQUENCE was accepted")
	}
	if _, err := readRequest(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Error("an empty reader should report EOF")
	}
}
