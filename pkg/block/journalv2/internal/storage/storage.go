package storage

// FS is one journal's directory. Names are of files directly inside it.
//
// Every call is one a crash-simulating implementation can record and discard:
// a Create, Rename or Remove made since the last SyncDir that returned nil is
// not durable, and a simulated crash undoes it.
type FS interface {
	// Create creates the file name, empty, and opens it for reading and
	// writing. It fails with an error matching fs.ErrExist if name exists.
	Create(name string) (File, error)

	// Open opens the existing file name for reading and writing. It fails
	// with an error matching fs.ErrNotExist if name does not exist. A file is
	// opened with sharing that permits it to be removed while open.
	Open(name string) (File, error)

	// Rename renames oldName to newName, replacing newName if it exists. The
	// rename is atomic, but a crash before SyncDir may undo it.
	Rename(oldName, newName string) error

	// Remove unlinks name.
	Remove(name string) error

	// List returns the names of the files in the directory, including those
	// the journal does not recognise.
	List() ([]string, error)

	// SyncDir makes every Create, Rename and Remove that returned before it
	// durable. A later success does not stand in for a SyncDir that failed.
	SyncDir() error
}

// File is one open file of a journal's directory.
//
// Every call is one a crash-simulating implementation can record and discard:
// a WriteAt, Allocate or Truncate made since the file's last Sync that
// returned nil is not durable, and a simulated crash discards it. A file
// allocated and never synced comes back short or unallocated.
type File interface {
	// ReadAt reads len(p) bytes at offset off, with the semantics of
	// io.ReaderAt.
	ReadAt(p []byte, off int64) (n int, err error)

	// ReadAtDirect reads as ReadAt does, but bypassing the page cache, so it
	// observes what reached the device.
	ReadAtDirect(p []byte, off int64) (n int, err error)

	// WriteAt writes len(p) bytes at offset off, with the semantics of
	// io.WriterAt.
	WriteAt(p []byte, off int64) (n int, err error)

	// Allocate allocates the n bytes at offset off, so a later WriteAt into
	// them needs no new space. Bytes no write has reached read as zeros.
	Allocate(off, n int64) error

	// Sync makes every WriteAt, Allocate and Truncate that returned before it
	// durable. It is a data-only sync, or a full sync where the platform
	// requires one. A later success does not stand in for a Sync that failed.
	Sync() error

	// Truncate sets the file's size to size.
	Truncate(size int64) error

	// Size returns the file's size in bytes.
	Size() (int64, error)

	// Close releases the file.
	Close() error

	// Name returns the name the file was created or opened under.
	Name() string
}
