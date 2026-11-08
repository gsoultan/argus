package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/apk"
	"github.com/gsoultan/argus/internal/storage"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Installing on Windows servers from the library.

An admin adds an MSI or an EXE. The control plane checks it is one (the
compound-file header of an MSI, a PE image for an EXE), reads who signed an
EXE (its Authenticode certificate: the name, and the SHA-256 of the
certificate), and keeps the arguments to run it with: MSI properties, or an
EXE's silent switches, without which an installer would wait for someone to
click Next.

A run is a package run with mode file_install. The agent fetches the file on
its own credential, from a path that opens for that run only while it is
queued or running, checks its SHA-256 against the one the console showed, and
runs msiexec /i ... /qn /norestart, or the EXE with its switches. Nothing is
passed through a shell.

Argus does not judge an installer: the SHA-256 pins the exact file a person
added, and the report says what it changed.
*/

// FileWindows is what the library knows about an MSI or an EXE.
type FileWindows struct {
	// Args are MSI properties (NAME=value) or an EXE's switches.
	Args []string `json:"args"`
	// Signer is the SHA-256 of the signing certificate, Subject its name;
	// both empty for a file nobody signed (or an MSI, whose signature
	// Argus does not read).
	Signer  string `json:"signer,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// FileRunOrder is the library file of a file_install run, as the agent
// reads it.
type FileRunOrder struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	SHA256 string   `json:"sha256"`
	Size   int64    `json:"size"`
	Args   []string `json:"args,omitempty"`
}

var (
	reMsiProperty = regexp.MustCompile(`^[A-Z][A-Z0-9_.]{0,63}=[^"\s]{0,200}$`)
	reExeArg      = regexp.MustCompile(`^[A-Za-z0-9/\-][A-Za-z0-9/\\\-_.:=+,@]{0,199}$`)
	msiMagic      = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
)

// parseWinArgs reads the arguments a person typed, one per space.
func parseWinArgs(kind, s string) ([]string, error) {
	args := strings.Fields(s)
	if len(args) > 20 {
		return nil, errors.New("at most 20 arguments")
	}
	for _, a := range args {
		switch {
		case kind == "msi" && !reMsiProperty.MatchString(a):
			return nil, fmt.Errorf("%q is not an MSI property: NAME=value, the name in capitals, no quotes or spaces", a)
		case kind == "exe" && !reExeArg.MatchString(a):
			return nil, fmt.Errorf("%q is not a switch Argus passes: letters, digits and / \\ - _ . : = + , @ only", a)
		}
	}
	if args == nil {
		args = []string{}
	}
	return args, nil
}

// readWinFile checks the file is the kind its name says and reads its
// signer where it can.
func readWinFile(r io.ReaderAt, size int64, kind string) (FileWindows, error) {
	head := make([]byte, 8)
	if _, err := r.ReadAt(head, 0); err != nil {
		return FileWindows{}, errors.New("the file is too short to be an installer")
	}
	var meta FileWindows
	switch kind {
	case "msi":
		if !bytes.Equal(head, msiMagic) {
			return meta, errors.New("this is not a Windows Installer package (.msi)")
		}
	case "exe":
		f, err := pe.NewFile(io.NewSectionReader(r, 0, size))
		if err != nil {
			return meta, errors.New("this is not a Windows program (.exe)")
		}
		defer f.Close()
		if f.FileHeader.Characteristics&pe.IMAGE_FILE_DLL != 0 {
			return meta, errors.New("this is a DLL, not an installer")
		}
		meta.Signer, meta.Subject = authenticodeSigner(r, size, f)
	default:
		return meta, errors.New("the library takes .msi and .exe for Windows")
	}
	return meta, nil
}

// authenticodeSigner reads the leaf certificate of a PE's Authenticode
// signature: the one that signs code and is not a CA. Empty when unsigned.
func authenticodeSigner(r io.ReaderAt, size int64, f *pe.File) (string, string) {
	var dd pe.DataDirectory
	switch h := f.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		if len(h.DataDirectory) > pe.IMAGE_DIRECTORY_ENTRY_SECURITY {
			dd = h.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]
		}
	case *pe.OptionalHeader64:
		if len(h.DataDirectory) > pe.IMAGE_DIRECTORY_ENTRY_SECURITY {
			dd = h.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]
		}
	}
	// The security directory's address is a file offset.
	if dd.Size < 8 || dd.Size > 1<<20 || int64(dd.VirtualAddress)+int64(dd.Size) > size {
		return "", ""
	}
	b := make([]byte, dd.Size)
	if _, err := r.ReadAt(b, int64(dd.VirtualAddress)); err != nil {
		return "", ""
	}
	// WIN_CERTIFICATE: length, revision, type (2 = PKCS#7 SignedData).
	n := binary.LittleEndian.Uint32(b[0:4])
	if n < 8 || int(n) > len(b) || binary.LittleEndian.Uint16(b[6:8]) != 2 {
		return "", ""
	}
	certs, err := apk.PKCS7Certificates(b[8:n])
	if err != nil {
		return "", ""
	}
	var pick []byte
	var subject string
	for _, der := range certs {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			continue
		}
		code := false
		for _, u := range c.ExtKeyUsage {
			code = code || u == x509.ExtKeyUsageCodeSigning
		}
		if !c.IsCA && (code || pick == nil) {
			pick, subject = der, c.Subject.CommonName
			if code {
				break
			}
		}
	}
	if pick == nil {
		return "", ""
	}
	sum := sha256.Sum256(pick)
	return hex.EncodeToString(sum[:]), subject
}

/* ── Running one ─────────────────────────────────────────────────────────── */

var reNotName = regexp.MustCompile(`[^A-Za-z0-9+._-]+`)

// fileItem is how a run names the file in its items: an id-safe slug, and
// the library name as its label.
func fileItem(f SoftwareFile) tunnel.SoftwareItem {
	slug := strings.Trim(reNotName.ReplaceAllString(f.Name, "-"), "-.")
	if slug == "" || !tunnel.ValidPackageName(slug) {
		slug = "file-" + f.SHA256[:12]
	}
	if len(slug) > 100 {
		slug = slug[:100]
	}
	return tunnel.SoftwareItem{Name: slug, Label: clip(f.Name, 200)}
}

// POST /api/v1/assets/{id}/software/file-runs {fileId}
func (a *API) postSoftwareFileRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "installing software", "admin", "owner") {
		return
	}
	var in struct {
		FileID string `json:"fileId"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, ok := a.softwareAsset(w, r)
	if !ok {
		return
	}
	if !hasMode(t.view.Installs, PackagesFileInstall) {
		writeErr(w, http.StatusConflict, "this server installs from its repositories or from a reviewed script; "+
			"MSI and EXE files are for Windows servers")
		return
	}
	f, err := a.store.softwareFile(r.Context(), in.FileID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "no such file in the library")
		return
	}
	if err != nil {
		a.fail(w, "software file", err)
		return
	}
	if f.Platform != "windows" || f.Windows == nil {
		writeErr(w, http.StatusBadRequest, "that library file is not a Windows installer")
		return
	}
	if isProduction(t.asset.Tags) {
		tried, err := a.store.fileTriedOutsideProduction(r.Context(), f.ID)
		if err != nil {
			a.fail(w, "software file", err)
			return
		}
		if !tried {
			writeErr(w, http.StatusConflict, f.Name+" has not installed on a server outside production yet; "+
				"install it on a test server first")
			return
		}
	}
	n := newRun{
		Hostname: t.agent, AssetHostname: t.asset.Hostname, Mode: PackagesFileInstall, Packages: []string{},
		Holds: []string{}, Reboot: "never", Trigger: "manual", State: RunQueued, RequestedBy: sess.Email,
		Items: []tunnel.SoftwareItem{fileItem(f)}, Expect: f.SHA256, SoftwareFileID: &f.ID,
	}
	if t.view.Rule.NeedsApproval {
		n.State = RunAwaitingApproval
	}
	runs, err := a.store.CreatePackageRuns(r.Context(), []newRun{n})
	if errors.Is(err, ErrRunActive) {
		writeErr(w, http.StatusConflict, "this server already has a change waiting or running; it does one at a time")
		return
	}
	if err != nil {
		a.fail(w, "software run", err)
		return
	}
	run := runs[0]
	run.AssetID = t.asset.ID
	extra := ""
	if run.State == RunAwaitingApproval {
		extra = "; it waits for another admin to approve it"
	}
	a.auditAsset(r, AuditEvent{Action: "software.file_requested", Severity: "notice", ActorEmail: sess.Email,
		Target: t.asset.Hostname, Detail: fmt.Sprintf("Asked for installing %s (%s, SHA-256 %s, arguments %q) on %s%s.",
			f.Name, f.FileName, f.SHA256, strings.Join(f.Windows.Args, " "), t.asset.Hostname, extra)})
	if run.State == RunAwaitingApproval {
		a.raiseAlert(r.Context(), ConsoleAlert{
			Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: sess.Email,
			Title:   sess.Email + " asks for " + runPhrase(run) + " on " + t.asset.Hostname,
			Detail:  "Approve or deny it on the server's Apps tab.",
			Link:    "/assets/" + t.asset.ID + "?tab=apps",
			Subject: packageRunSubject(run.ID),
		})
	}
	writeJSON(w, http.StatusAccepted, run)
}

func hasMode(modes []string, m string) bool {
	for _, x := range modes {
		if x == m {
			return true
		}
	}
	return false
}

// validInstalls keeps the modes an agent may say it does.
func validInstalls(in []string) []string {
	var out []string
	for _, m := range in {
		switch m {
		case PackagesRemove, PackagesScript, PackagesFileInstall:
			if !hasMode(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

// isWindowsAsset is a server the console reaches over RDP or that says it
// runs Windows.
func isWindowsAsset(as Asset) bool {
	return as.Kind != "android" && (as.Protocol == "rdp" || strings.Contains(strings.ToLower(as.OS), "windows"))
}

// fileTriedOutsideProduction is whether the file has finished installing on
// a server without a production tag, which it must before production.
func (s *Store) fileTriedOutsideProduction(ctx context.Context, id string) (bool, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT hostname FROM package_runs
		WHERE software_file_id = $1::uuid AND mode = 'file_install' AND state = 'done'`, id)
	if err != nil {
		return false, err
	}
	var hosts []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return false, err
		}
		hosts = append(hosts, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, h := range hosts {
		_, _, tags, err := s.assetForAgent(ctx, h)
		if err == nil && !isProduction(tags) {
			return true, nil
		}
	}
	return false, nil
}

// attachFile fills the file half of a file_install run on its way to the
// agent. A run that cannot be sent fails here.
func (a *API) attachFile(ctx context.Context, host string, o *PackageRunOrder) bool {
	if o == nil || o.Mode != PackagesFileInstall {
		return true
	}
	order, err := a.fileOrder(ctx, o)
	if err == nil {
		o.File = order
		return true
	}
	a.log.Error("heartbeat: cannot send a file install", "run", o.ID, "error", err)
	if r, ferr := a.store.failUnsentRun(ctx, host, o.ID, err.Error()); ferr == nil && r != nil {
		a.packageRunEnded(ctx, *r)
	}
	return false
}

func (a *API) fileOrder(ctx context.Context, o *PackageRunOrder) (*FileRunOrder, error) {
	var fileID *string
	if err := a.store.db(ctx).QueryRow(ctx, `SELECT software_file_id::text FROM package_runs WHERE id = $1::uuid`,
		o.ID).Scan(&fileID); err != nil {
		return nil, err
	}
	if fileID == nil {
		return nil, errors.New("the install names no library file")
	}
	f, err := a.store.softwareFileAny(ctx, *fileID)
	if err != nil {
		return nil, err
	}
	switch {
	case f.archived:
		return nil, errors.New(f.FileName + " was removed from the software library")
	case f.Windows == nil || f.SHA256 != o.Expect:
		return nil, errors.New("the library file is not the one asked for")
	}
	return &FileRunOrder{Kind: f.Kind, Name: f.Name, Path: "/api/v1/report/software-file/" + o.ID,
		SHA256: f.SHA256, Size: f.Size, Args: f.Windows.Args}, nil
}

func winMeta(f FileWindows) ([]byte, error) { return json.Marshal(f) }

// addWindowsFile keeps an uploaded MSI or EXE: ?name=, ?args= and ?note=.
func (a *API) addWindowsFile(w http.ResponseWriter, r *http.Request, by string, store *storage.Scoped, tmp *os.File,
	name, ext string, size int64, sum string) {
	kind := strings.TrimPrefix(ext, ".")
	meta, err := readWinFile(tmp, size, kind)
	if err != nil {
		writeErr(w, http.StatusBadRequest, name+": "+err.Error())
		return
	}
	q := r.URL.Query()
	if meta.Args, err = parseWinArgs(kind, q.Get("args")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if kind == "exe" && len(meta.Args) == 0 {
		writeErr(w, http.StatusBadRequest, "an EXE installer needs its silent switches (often /S, /quiet or /VERYSILENT): "+
			"Argus runs it with nobody there to click Next")
		return
	}
	if existing, err := a.store.softwareFileBySHA(r.Context(), sum); err == nil {
		writeJSON(w, http.StatusOK, existing)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		a.fail(w, "software file", err)
		return
	}
	display := strings.TrimSpace(q.Get("name"))
	if display == "" {
		display = strings.TrimSuffix(name, filepath.Ext(name))
	}
	f := SoftwareFile{Platform: "windows", Kind: kind, Name: clip(display, 120), FileName: clip(name, 255), SHA256: sum,
		Size: size, Windows: &meta, Note: clip(strings.TrimSpace(q.Get("note")), 500), CreatedBy: by}
	switch {
	case kind == "msi":
		f.Warnings = append(f.Warnings, "Argus does not read an MSI's signature: check it is the vendor's before servers install it. "+
			"Its SHA-256 pins this exact file.")
	case meta.Signer == "":
		f.Warnings = append(f.Warnings, "This EXE is not signed. Its SHA-256 pins this exact file, but nothing says who made it.")
	}
	raw, err := winMeta(meta)
	if err != nil {
		a.fail(w, "software file", err)
		return
	}
	key, err := store.PutKey(r.Context(), tmp.Name(), "software/"+sum+ext)
	if err != nil {
		a.fail(w, "store software file", err)
		return
	}
	f.storageKey = key
	if err := a.store.insertSoftwareFile(r.Context(), &f, raw); err != nil {
		a.fail(w, "software file", err)
		return
	}
	signed := "unsigned"
	if meta.Subject != "" {
		signed = "signed by " + meta.Subject
	} else if kind == "msi" {
		signed = "signature not read"
	}
	a.auditAsset(r, AuditEvent{Action: "software.file_added", Severity: "notice", ActorEmail: by, Target: f.Name,
		Detail: fmt.Sprintf("Added %s to the software library for Windows (%s), run with %q, SHA-256 %s.",
			f.FileName, signed, strings.Join(meta.Args, " "), sum)})
	writeJSON(w, http.StatusCreated, f)
}
