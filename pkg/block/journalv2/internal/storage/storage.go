package storage

// FS is one journal's directory. Names are of files directly inside it, never
// paths: a name holding a path separator is refused with fs.ErrInvalid.
//
// Every call is one a crash-simulating implementation can record and discard:
// a Create, Rename or Remove made since the last SyncDir that returned nil is
// not durable, and a simulated crash undoes it.
type FS interface {
	// Create creates the file name, empty, and opens it for reading and
	// writing. It fails with an error matching fs.ErrExist if name exists,
	// and creates nothing.
	Create(name string) (File, error)

	// Open opens the existing file name for reading and writing. It fails
	// with an error matching fs.ErrNotExist if name does not exist.
	//
	// decision: a file is opened with sharing that permits it to be removed
	// or renamed while open, on every platform, so a segment can be unlinked
	// while a reader still holds it.
	Open(name string) (File, error)

	// Rename renames oldName to newName, replacing newName if it exists. The
	// replacement is atomic: no observer sees neither name.
	Rename(oldName, newName string) error

	// Remove unlinks name. A descriptor already open on it stays usable until
	// closed. It fails with an error matching fs.ErrNotExist if name does not
	// exist.
	Remove(name string) error

	// List returns the names of the files in the directory, in no order. It
	// lists every file, those the journal does not recognise included.
	List() ([]string, error)

	// SyncDir makes every Create, Rename and Remove that returned before it
	// durable. A failed SyncDir leaves them not known to be durable, and a
	// later success does not make them durable retroactively for a caller
	// that already saw the failure.
	SyncDir() error
}

// File is one open file of a journal's directory. Its methods are safe for
// concurrent use on disjoint ranges.
//
// Every call is one a crash-simulating implementation can record and discard:
// a WriteAt, Allocate or Truncate made since the file's last Sync that
// returned nil is not durable, and a simulated crash discards it. A file
// allocated and never synced comes back short or unallocated.
type File interface {
	// ReadAt reads len(p) bytes at offset off, with the semantics of
	// io.ReaderAt. It may be served from the operating system's page cache.
	ReadAt(p []byte, off int64) (n int, err error)

	// ReadAtDirect reads as ReadAt does, but from the device, bypassing the
	// page cache, so it observes what survived a power loss rather than what
	// the cache still holds.
	//
	// decision: alignment is the implementation's concern. The platform's
	// uncached read may require an aligned buffer, offset and length; the
	// implementation reads an aligned range around the request and copies,
	// so callers stay free of platform rules.
	ReadAtDirect(p []byte, off int64) (n int, err error)

	// WriteAt writes len(p) bytes at offset off, with the semantics of
	// io.WriterAt. The bytes are not durable until Sync returns nil.
	WriteAt(p []byte, off int64) (n int, err error)

	// Allocate reserves storage for the n bytes at offset off, so a later
	// WriteAt into them needs the filesystem to find no space. Bytes no write
	// has reached read as zeros. The file's size becomes at least off + n.
	Allocate(off, n int64) error

	// Sync makes every WriteAt, Allocate and Truncate that returned before it
	// durable. It is a data-only sync where the platform's data-only sync
	// reaches the device, and a full sync where only that does. A failed Sync
	// leaves them not known to be durable, and a later success does not make
	// them durable for a caller that already saw the failure.
	Sync() error

	// Truncate sets the file's size to size, freeing storage past it.
	Truncate(size int64) error

	// Size returns the file's size in bytes.
	Size() (int64, error)

	// Close releases the descriptor. Calling any method after Close fails
	// with an error matching fs.ErrClosed.
	Close() error

	// Name returns the name the file was created or opened under, as given.
	Name() string
}
