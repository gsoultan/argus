package vault

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RootKeyCredential is the name argus-secret looks for in systemd's
// $CREDENTIALS_DIRECTORY. With LoadCredentialEncrypted= in the unit, systemd
// decrypts it at start, with the TPM and/or the host key, so the root key is
// never on disk in the clear:
//
//	systemd-creds encrypt --with-key=tpm2 --name=argus-secret-root-key \
//	    /var/lib/argus-secret/root.key /etc/credstore.encrypted/argus-secret-root-key
//	shred -u /var/lib/argus-secret/root.key
const RootKeyCredential = "argus-secret-root-key"

// rootKeyCommandTimeout bounds root_key_command: a KMS that does not answer
// must not hold the server's start forever.
const rootKeyCommandTimeout = 30 * time.Second

// RootKeySource says where the root key came from, for the start log.
type RootKeySource string

const (
	RootKeyFromCommand    RootKeySource = "root_key_command"
	RootKeyFromCredential RootKeySource = "systemd credential"
	RootKeyFromFile       RootKeySource = "root_key_file"
)

// LoadConfiguredRootKey gets the root key in this order: root_key_command
// (a KMS, a TPM tool, a hardware token), a systemd credential, then
// root_key_file, made on first start when no other source is set and the
// file is missing.
func LoadConfiguredRootKey(c Config) (key []byte, src RootKeySource, created bool, err error) {
	if len(c.RootKeyCommand) > 0 {
		key, err = rootKeyFromCommand(c.RootKeyCommand)
		return key, RootKeyFromCommand, false, err
	}
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		p := filepath.Join(dir, RootKeyCredential)
		if raw, err := os.ReadFile(p); err == nil {
			key, err = decodeRootKey(raw)
			if err != nil {
				return nil, RootKeyFromCredential, false, fmt.Errorf("systemd credential %s: %w", RootKeyCredential, err)
			}
			return key, RootKeyFromCredential, false, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, RootKeyFromCredential, false, err
		}
	}
	key, created, err = LoadRootKey(c.RootKeyFile)
	return key, RootKeyFromFile, created, err
}

// rootKeyFromCommand runs argv (no shell) and reads the key from its output.
// What it writes to stderr is not shown: it may say too much about the key.
func rootKeyFromCommand(argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rootKeyCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("root_key_command did not finish within %s", rootKeyCommandTimeout)
		}
		return nil, fmt.Errorf("root_key_command %s failed: %w", argv[0], err)
	}
	raw := out.Bytes()
	defer clear(raw)
	key, err := decodeRootKey(raw)
	if err != nil {
		return nil, fmt.Errorf("root_key_command: %w", err)
	}
	return key, nil
}

// decodeRootKey reads 32 raw bytes, or base64 of them (standard or URL
// alphabet), with surrounding white space allowed for base64.
func decodeRootKey(raw []byte) ([]byte, error) {
	if len(raw) == KeySize {
		return bytes.Clone(raw), nil
	}
	s := strings.TrimSpace(string(raw))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == KeySize {
			return b, nil
		}
	}
	return nil, fmt.Errorf("the root key is %d raw bytes or their base64, not %d bytes", KeySize, len(raw))
}
