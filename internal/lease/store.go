package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Header carries the serve-side settings that client commands read from the
// state file.
type Header struct {
	Patterns   []string      `json:"patterns"`
	DefaultTTL time.Duration `json:"default_ttl"`
	// RemoveAfter is how long a stopped project is kept before removal;
	// 0 means never remove. Status and list need it to render the removal
	// time of a stopped project.
	RemoveAfter time.Duration `json:"remove_after,omitempty"`
}

// File is the whole on-disk state: a header plus one lease per project.
type File struct {
	Version int              `json:"version"`
	Header  Header           `json:"header"`
	Leases  map[string]Lease `json:"leases"`
}

// Store is one JSON state file guarded by an exclusive lock on Path+".lock".
// Every read and write takes the lock; writes go to a temp file in the same
// directory and are renamed into place, so a reader never sees a half-written
// file. A file that does not parse returns ErrCorrupt and is never
// overwritten.
type Store struct {
	Path string
}

func (s Store) lockPath() string { return s.Path + ".lock" }

// Update reads the state, calls fn with the leases map and writes the state
// back. Nothing is written when fn returns an error.
func (s Store) Update(fn func(map[string]Lease) error) error {
	return s.update(func(f *File) error { return fn(f.Leases) })
}

// UpdateHeader is Update for the header part of the state file.
func (s Store) UpdateHeader(fn func(*Header) error) error {
	return s.update(func(f *File) error { return fn(&f.Header) })
}

func (s Store) update(fn func(*File) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.readFile()
	if err != nil {
		return err
	}
	if f.Leases == nil {
		f.Leases = map[string]Lease{}
	}
	if err := fn(f); err != nil {
		return err
	}
	return s.writeFile(f)
}

// Read returns the leases of the state file, or an empty map when the file
// does not exist yet.
func (s Store) Read() (map[string]Lease, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	f, err := s.readFile()
	if err != nil {
		return nil, err
	}
	return f.Leases, nil
}

// ReadHeader returns the header of the state file.
func (s Store) ReadHeader() (Header, error) {
	unlock, err := s.lock()
	if err != nil {
		return Header{}, err
	}
	defer unlock()
	f, err := s.readFile()
	if err != nil {
		return Header{}, err
	}
	return f.Header, nil
}

// lock returns a function releasing the exclusive lock on the lock file.
// The implementation is per-platform (see lock_unix.go / lock_other.go).

func (s Store) readFile() (*File, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return &File{Version: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, f.Version)
	}
	if f.Leases == nil {
		f.Leases = map[string]Lease{}
	}
	return &f, nil
}

func (s Store) writeFile(f *File) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.Path)
	tmp, err := os.CreateTemp(dir, ".leases-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
