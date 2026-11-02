package tunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

/*
The backup service: a dump or a restore of a database, run by the agent on
the database's own server, on a grant the control plane signed for exactly
that job.

	argus-control ──"backup" stream──▶ agent ──runAs postgres──▶ pg_dump / pg_restore

The control plane opens one stream per job. After the header and the answer,
frames are:

	toward the agent     'q' BackupRequest (JSON): cancel; or, for a restore
	                         that streams from the control plane, nothing
	                         (the archive follows as 'd' frames, then 'e')
	                     'd' a piece of the archive (restore from a stream)
	                     'e' end of the archive
	toward the plane     'p' BackupProgress (JSON), every couple of seconds
	                     'd' a piece of the archive (dump to the control plane)
	                     'e' end of the archive
	                     'z' BackupResult (JSON): the job's outcome, last

A grant is for one job: a dump grant carries the public key to encrypt to
and nothing that could decrypt; a restore grant carries the one data key
that opens the one backup it names. ForService keeps a backup grant from
opening a workspace and a workspace grant from starting a job.
*/

// BackupService is the tunnel service name.
const BackupService = "backup"

// Backup frame types.
const (
	FrameBackupRequest  byte = 'q'
	FrameBackupProgress byte = 'p'
	FrameBackupData     byte = 'd'
	FrameBackupEnd      byte = 'e'
	FrameBackupResult   byte = 'z'
)

// BackupDataChunk is the size of one 'd' frame: well under MaxFrame, and
// large enough that a gigabyte is a few thousand frames, not a million.
const BackupDataChunk = 128 << 10

// Backup jobs.
const (
	BackupJobDump    = "dump"
	BackupJobRestore = "restore"
	BackupJobCatalog = "catalog"
	// BackupJobPrune deletes backups retention no longer keeps on the server.
	BackupJobPrune = "prune"
	// BackupJobDrop drops the copy a replace kept, once its day is up;
	// BackupJobUndo puts that copy back in place of the restored database.
	BackupJobDrop = "drop"
	BackupJobUndo = "undo"
)

// KeptMarker is in every name a replaced database is renamed to, so a drop
// or an undo can refuse anything else.
const KeptMarker = "_before_restore_"

// Backup destinations.
const (
	BackupDestServer  = "server"
	BackupDestControl = "control"
	BackupDestBoth    = "both"
)

// Restore modes.
const (
	RestoreNew     = "new"
	RestoreReplace = "replace"
)

// GrantBackup is the job a backup grant allows; nil on every other grant.
type GrantBackup struct {
	Job string `json:"job"`
	// BackupID names the backup being written or read. For a restore it is
	// bound into the archive header, so a grant for backup A cannot be spent
	// on backup B.
	BackupID string `json:"backupId,omitempty"`
	// Organization is bound into the archive header too.
	Organization string `json:"org,omitempty"`
	// The database on this server, as a postgres grant names it.
	DatabaseID string `json:"databaseId"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	AdminUser  string `json:"adminUser,omitempty"`
	// Destination is where a dump goes: server, control or both.
	Destination string `json:"destination,omitempty"`
	// PublicKey is the organization's backup key, for a dump. The agent can
	// encrypt to it and nothing else.
	PublicKey string `json:"publicKey,omitempty"`
	// DataKey opens the one backup a restore grant names; base64. SHA256 is
	// what the archive must hash to before it is opened.
	DataKey string `json:"dataKey,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	// Restore is how the archive is applied.
	Restore *GrantRestore `json:"restore,omitempty"`
	// Prune lists the backup ids whose local files should go.
	Prune []string `json:"prune,omitempty"`
	// Kept is the renamed copy a drop or undo job acts on.
	Kept string `json:"kept,omitempty"`
	// MinFreeBytes refuses a dump that would leave less than this free.
	MinFreeBytes int64 `json:"minFreeBytes,omitempty"`
	// Reason is why, shown in the agent's log and the archive header.
	Reason string `json:"reason,omitempty"`
}

// GrantRestore is how a restore applies the archive.
type GrantRestore struct {
	// Mode is new (into NewName) or replace (over the grant's database).
	Mode    string `json:"mode"`
	NewName string `json:"newName,omitempty"`
	// ApprovalID is the approved request a replace rests on. The agent
	// refuses a replace without one.
	ApprovalID string `json:"approvalId,omitempty"`
	// Source is local (the file is on this server) or stream (the control
	// plane sends it after the answer).
	Source string `json:"source"`
	// RenameTo is where the replaced database goes; the agent uses it as
	// given so the control plane knows the name without a round trip.
	RenameTo string `json:"renameTo,omitempty"`
}

// BackupRequest is what the control plane may ask of a running job.
type BackupRequest struct {
	// Op is cancel.
	Op string `json:"op"`
}

// BackupProgress is where a job is.
type BackupProgress struct {
	// Stage is a short word for the console: starting, schema, data,
	// verifying, uploading, restoring, finishing.
	Stage string `json:"stage"`
	// Detail is the table being dumped, or the step of a restore.
	Detail string `json:"detail,omitempty"`
	// Bytes is the archive written so far (ciphertext), or read for a restore.
	Bytes int64 `json:"bytes"`
	// ElapsedMS is since the job started.
	ElapsedMS int64 `json:"elapsedMs"`
}

// BackupResult is how a job ended.
type BackupResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Stage is where it failed.
	Stage string `json:"stage,omitempty"`
	// For a dump: what was written.
	Bytes      int64  `json:"bytes,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	WrappedKey string `json:"wrappedKey,omitempty"`
	KeyID      string `json:"keyId,omitempty"`
	LocalPath  string `json:"localPath,omitempty"`
	Verified   bool   `json:"verified,omitempty"`
	// Versions as the tools reported them.
	ServerVersion string `json:"serverVersion,omitempty"`
	DumpVersion   string `json:"dumpVersion,omitempty"`
	Compression   string `json:"compression,omitempty"`
	// For a restore: pg_restore's non-fatal errors, and where the replaced
	// database went.
	Warnings  int    `json:"warnings,omitempty"`
	RenamedTo string `json:"renamedTo,omitempty"`
	// For a catalog: what the server has.
	Catalog *BackupCatalog `json:"catalog,omitempty"`
	// For a prune: what was removed.
	Pruned []string `json:"pruned,omitempty"`
}

// BackupCatalog is what a server can do.
type BackupCatalog struct {
	DumpVersion    string `json:"dumpVersion"`
	RestoreVersion string `json:"restoreVersion"`
	ServerVersion  string `json:"serverVersion"`
	// FreeBytes under the backup directory.
	FreeBytes int64  `json:"freeBytes"`
	Dir       string `json:"dir"`
	// Compression is what pg_dump will use: zstd on 16+, gzip before.
	Compression string `json:"compression"`
	// Local lists the backups on the server's disk by id, with sizes.
	Local map[string]int64 `json:"local,omitempty"`
}

// ValidDBName is a Postgres database name Argus will create: lower-case,
// digits and underscores, up to 63 bytes, not starting with a digit. Narrower
// than Postgres allows, so it is never quoted and never surprising.
var ValidDBName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ValidBackupID is a UUID as the control plane prints it, which is the only
// thing a backup id is ever used as on the server: a file name.
var ValidBackupID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Validate says whether a backup grant payload is one the agent may act on.
func (b *GrantBackup) Validate() error {
	if b == nil {
		return errors.New("it is not for a backup")
	}
	switch b.Job {
	case BackupJobDump, BackupJobRestore, BackupJobCatalog, BackupJobPrune, BackupJobDrop, BackupJobUndo:
	default:
		return fmt.Errorf("unknown backup job %q", b.Job)
	}
	if !LocalPGHost(b.Host) {
		return fmt.Errorf("the database must be on this server; %s is not", b.Host)
	}
	if b.Port <= 0 || b.Port > 65535 {
		return errors.New("the grant does not say which port")
	}
	if b.Job != BackupJobCatalog && b.Job != BackupJobPrune && !ValidDBName.MatchString(b.Name) {
		return fmt.Errorf("not a database name Argus will act on: %q", b.Name)
	}
	if b.AdminUser != "" && !safeAccount.MatchString(b.AdminUser) {
		return fmt.Errorf("not an account name: %q", b.AdminUser)
	}
	switch b.Job {
	case BackupJobDump:
		if !ValidBackupID.MatchString(b.BackupID) {
			return errors.New("the grant does not name the backup it writes")
		}
		switch b.Destination {
		case BackupDestServer, BackupDestControl, BackupDestBoth:
		default:
			return fmt.Errorf("unknown destination %q", b.Destination)
		}
		if _, err := DecodeBackupPublicKey(b.PublicKey); err != nil {
			return err
		}
	case BackupJobRestore:
		if !ValidBackupID.MatchString(b.BackupID) {
			return errors.New("the grant does not name the backup it restores")
		}
		if b.Restore == nil {
			return errors.New("the grant does not say how to restore")
		}
		switch b.Restore.Mode {
		case RestoreNew:
			if !ValidDBName.MatchString(b.Restore.NewName) {
				return fmt.Errorf("not a database name Argus will create: %q", b.Restore.NewName)
			}
			if b.Restore.NewName == b.Name {
				return errors.New("a restore into a new database cannot use the existing name")
			}
		case RestoreReplace:
			if b.Restore.ApprovalID == "" {
				return errors.New("replacing a database needs an approval, and the grant names none")
			}
			if !ValidDBName.MatchString(b.Restore.RenameTo) || b.Restore.RenameTo == b.Name {
				return errors.New("the grant does not say where the replaced database goes")
			}
		default:
			return fmt.Errorf("unknown restore mode %q", b.Restore.Mode)
		}
		if b.Restore.Source != "local" && b.Restore.Source != "stream" {
			return fmt.Errorf("unknown restore source %q", b.Restore.Source)
		}
		if len(b.DataKey) == 0 || len(b.SHA256) != 64 {
			return errors.New("the grant does not carry the key and hash of the backup")
		}
	case BackupJobDrop, BackupJobUndo:
		if !ValidDBName.MatchString(b.Kept) || !strings.Contains(b.Kept, KeptMarker) {
			return errors.New("not a copy Argus kept: " + b.Kept)
		}
	case BackupJobPrune:
		for _, id := range b.Prune {
			if !ValidBackupID.MatchString(id) {
				return fmt.Errorf("not a backup id: %q", id)
			}
		}
	}
	return nil
}

var safeAccount = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// MaskConnInfo hides the password in a libpq connection string, for anything
// that is shown, recorded or stored. Everything else is left as it is.
func MaskConnInfo(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		if strings.HasPrefix(strings.ToLower(p), "password=") {
			parts[i] = "password=***"
		}
	}
	return strings.Join(parts, " ")
}

// EncodeBackupMessage and DecodeBackupMessage are the JSON frames.
func EncodeBackupMessage(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
