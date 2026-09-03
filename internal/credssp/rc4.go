package credssp

import "crypto/rc4"

// rc4Crypt encrypts or decrypts under a fresh RC4 stream.
//
// RC4 is broken and is used because CredSSP specifies it. A fresh cipher per
// call is required: reusing one across messages would reuse keystream, which is
// how a stream cipher leaks plaintext outright.
func rc4Crypt(key, data []byte) []byte {
	c, err := rc4.NewCipher(key)
	if err != nil {
		// Only returned for an invalid key size, which is fixed at 16 here.
		panic("credssp: invalid RC4 key size: " + err.Error())
	}
	out := make([]byte, len(data))
	c.XORKeyStream(out, data)
	return out
}
