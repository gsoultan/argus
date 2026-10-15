package agent

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

/*
Logging an account on without its password.

A session opened through the tunnel was authorised by the control plane,
which signed a grant naming the account; the agent has no password for it
and must not need one. Windows calls this S4U ("service for user"): a
service holding the TCB privilege -- the agent runs as LocalSystem -- asks
LSA for a token for any account, and gets one with that account's groups and
privileges, as if it had logged on. The token carries no credentials, so
the session cannot reach network shares as the account; it is the same
limit an OpenSSH key login has.

Only local accounts for now, through MSV1_0. A domain account would go
through Kerberos with the same request shape, but nothing tests that yet, so
it is refused rather than half-supported.
*/

var (
	modSecur32                  = windows.NewLazySystemDLL("secur32.dll")
	procLsaRegisterLogonProcess = modSecur32.NewProc("LsaRegisterLogonProcess")
	procLsaLookupAuthPackage    = modSecur32.NewProc("LsaLookupAuthenticationPackage")
	procLsaLogonUser            = modSecur32.NewProc("LsaLogonUser")
	procLsaFreeReturnBuffer     = modSecur32.NewProc("LsaFreeReturnBuffer")
	procLsaDeregisterLogonProc  = modSecur32.NewProc("LsaDeregisterLogonProcess")
	modAdvapi32                 = windows.NewLazySystemDLL("advapi32.dll")
	procLsaNtStatusToWinError   = modAdvapi32.NewProc("LsaNtStatusToWinError")
	procAllocateLUID            = modAdvapi32.NewProc("AllocateLocallyUniqueId")
)

// lsaString is LSA_STRING: an ANSI string with lengths in bytes.
type lsaString struct {
	Length, MaximumLength uint16
	Buffer                *byte
}

func newLSAString(s string) lsaString {
	b := append([]byte(s), 0)
	return lsaString{Length: uint16(len(s)), MaximumLength: uint16(len(b)), Buffer: &b[0]}
}

// s4uLogon is MSV1_0_S4U_LOGON. Its strings must live in the same buffer,
// after it.
type s4uLogon struct {
	MessageType uint32
	Flags       uint32
	User        windows.NTUnicodeString
	Domain      windows.NTUnicodeString
}

// MsV1_0S4ULogon (ntsecapi.h).
const s4uLogonMessage = 12

type tokenSource struct {
	SourceName       [8]byte
	SourceIdentifier windows.LUID
}

const logonNetwork = 3

func ntError(status uintptr) error {
	r, _, _ := procLsaNtStatusToWinError.Call(status)
	return syscall.Errno(r)
}

// s4uToken logs account on and returns a primary token for it. account is
// a name as a grant carries it: "name", or `COMPUTER\name`.
func s4uToken(account string) (windows.Token, error) {
	user, domain, err := splitAccount(account)
	if err != nil {
		return 0, err
	}

	var lsa windows.Handle
	var mode uint32
	name := newLSAString("Argus")
	if st, _, _ := procLsaRegisterLogonProcess.Call(uintptr(unsafe.Pointer(&name)),
		uintptr(unsafe.Pointer(&lsa)), uintptr(unsafe.Pointer(&mode))); st != 0 {
		return 0, fmt.Errorf("cannot reach the local security authority "+
			"(the agent must run as LocalSystem): %w", ntError(st))
	}
	defer procLsaDeregisterLogonProc.Call(uintptr(lsa))

	const pkgName = "MICROSOFT_AUTHENTICATION_PACKAGE_V1_0"
	pkg := newLSAString(pkgName)
	var pkgID uint32
	if st, _, _ := procLsaLookupAuthPackage.Call(uintptr(lsa),
		uintptr(unsafe.Pointer(&pkg)), uintptr(unsafe.Pointer(&pkgID))); st != 0 {
		return 0, fmt.Errorf("cannot find the %s logon package: %w", pkgName, ntError(st))
	}

	auth, size := s4uBuffer(user, domain)

	var src tokenSource
	copy(src.SourceName[:], "Argus")
	if r, _, err := procAllocateLUID.Call(uintptr(unsafe.Pointer(&src.SourceIdentifier))); r == 0 {
		return 0, err
	}
	origin := newLSAString("Argus tunnel session")
	var (
		profile    uintptr
		profileLen uint32
		logonID    windows.LUID
		token      windows.Token
		quotas     [6]uintptr // QUOTA_LIMITS
		subStatus  uintptr
	)
	st, _, _ := procLsaLogonUser.Call(uintptr(lsa), uintptr(unsafe.Pointer(&origin)),
		logonNetwork, uintptr(pkgID), uintptr(unsafe.Pointer(&auth[0])), uintptr(size),
		0, uintptr(unsafe.Pointer(&src)),
		uintptr(unsafe.Pointer(&profile)), uintptr(unsafe.Pointer(&profileLen)),
		uintptr(unsafe.Pointer(&logonID)), uintptr(unsafe.Pointer(&token)),
		uintptr(unsafe.Pointer(&quotas)), uintptr(unsafe.Pointer(&subStatus)))
	if profile != 0 {
		procLsaFreeReturnBuffer.Call(profile)
	}
	if st != 0 {
		err := ntError(st)
		if subStatus != 0 {
			err = fmt.Errorf("%w (%w)", err, ntError(subStatus))
		}
		return 0, fmt.Errorf("Windows refused to log %s on: %w", account, err)
	}
	defer token.Close()

	var primary windows.Token
	if err := windows.DuplicateTokenEx(token, windows.MAXIMUM_ALLOWED, nil,
		windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return 0, err
	}
	return primary, nil
}

// s4uBuffer lays the request out as LSA wants it: the struct, then its
// strings, in one allocation, with pointers into it.
func s4uBuffer(user, domain string) ([]byte, int) {
	u := windows.StringToUTF16(user)
	d := windows.StringToUTF16(domain)
	head := int(unsafe.Sizeof(s4uLogon{}))
	size := head + 2*len(u) + 2*len(d)
	buf := make([]byte, size)
	req := (*s4uLogon)(unsafe.Pointer(&buf[0]))
	req.MessageType = s4uLogonMessage
	put := func(dst *windows.NTUnicodeString, s []uint16, off int) int {
		p := (*uint16)(unsafe.Pointer(&buf[off]))
		copy(unsafe.Slice(p, len(s)), s)
		dst.Buffer = p
		dst.Length = uint16(2 * (len(s) - 1)) // without the terminator
		dst.MaximumLength = uint16(2 * len(s))
		return off + 2*len(s)
	}
	off := put(&req.User, u, head)
	put(&req.Domain, d, off)
	return buf, size
}

// splitAccount reads an account name. A bare name, or one qualified with
// this computer's name or ".", is a local account.
func splitAccount(account string) (user, domain string, err error) {
	computer, err := windows.ComputerName()
	if err != nil {
		return "", "", err
	}
	user = account
	if i := strings.IndexByte(account, '\\'); i >= 0 {
		if d := account[:i]; d != "." && !strings.EqualFold(d, computer) {
			return "", "", errDomainAccount
		}
		user = account[i+1:]
	}
	if strings.ContainsAny(user, `\@/`) || user == "" {
		return "", "", errDomainAccount
	}
	return user, computer, nil
}

var errDomainAccount = errors.New("only local accounts can be opened through the " +
	"tunnel on Windows for now; domain accounts are not supported yet")
