package tunnel

/*
The files service's frames, after the header and answer.

The same framing as the terminal: type, length, payload. One operation at a
time on a stream, in the order the control plane asks for them:

	list      'q' {op:list, path}               → 'a' {entries}
	download  'q' {op:download, path}           → 'a' {size}, 'd'…, 'a' {sha256}
	upload    'q' {op:upload, path, overwrite}  → 'a' {}, then 'd'…, 'e' → 'a' {size, sha256}

Every answer carries ok or an error; an error ends that operation and not the
stream, so a typo in a path does not cost the operator their file browser.
*/

// Files frame types.
const (
	FrameFileRequest byte = 'q' // FileRequest, toward the agent
	FrameFileAnswer  byte = 'a' // FileAnswer, toward the control plane
	FrameFileData    byte = 'd' // file bytes, either way
	FrameFileEnd     byte = 'e' // the end of an upload's bytes
)

// FileChunk is how much file data one frame carries.
const FileChunk = 128 << 10

// MaxUpload bounds one upload. A browser holds the whole file to send it,
// and this is a console for administering servers, not a bulk-copy tool.
const MaxUpload int64 = 2 << 30

// MaxListing bounds a directory listing.
const MaxListing = 5000

// FileRequest is one operation.
type FileRequest struct {
	Op   string `json:"op"` // list, download, upload
	Path string `json:"path"`
	// Overwrite lets an upload replace a file that exists.
	Overwrite bool `json:"overwrite,omitempty"`
}

// FileEntry is one item in a listing.
type FileEntry struct {
	Name    string `json:"name"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mtime"`
	// Link is set for a symbolic link, to where it points.
	Link string `json:"link,omitempty"`
}

// FileAnswer is the agent's reply to a FileRequest, or the end of one.
type FileAnswer struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Path is the absolute path the operation used, after ~ and relative
	// paths were resolved against the account's home.
	Path    string      `json:"path,omitempty"`
	Entries []FileEntry `json:"entries,omitempty"`
	// Truncated says a listing stopped at MaxListing entries.
	Truncated bool   `json:"truncated,omitempty"`
	Size      int64  `json:"size,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}
