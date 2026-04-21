// Package store defines storage interfaces and a filesystem-backed
// implementation used by the agent runtime and 9P server.
package store

import (
	"os"
	"path/filepath"
	"time"
)

// ReadableStore is a named collection of byte blobs supporting only read operations.
type ReadableStore interface {
	Stat(name string) (os.FileInfo, error)
	List() ([]os.DirEntry, error)
	Get(name string) ([]byte, error)
}

// Runnable is a running agent that can be observed and controlled.
type Runnable interface {
	RunnableID() string
	Cancel()
	Interrupt()
	AppendLog([]byte)
	LogInfo() (length int, vers uint32)
}

// WritableStore supports writing blobs by name.
type WritableStore interface {
	Put(name string, data []byte) error
}

// ReadWriteStore supports reading and writing but not structural mutations.
type ReadWriteStore interface {
	ReadableStore
	WritableStore
}

// BlobStore extends ReadWriteStore with creation and deletion.
type BlobStore interface {
	ReadWriteStore
	Delete(name string) error
	Create(name string) error
}

// Store is a fully mutable store that also supports renaming entries.
type Store interface {
	BlobStore
	Rename(oldName, newName string) error
}

// storeConfig implements the store interfaces via function pointers.
type storeConfig struct {
	StatFn   func(string) (os.FileInfo, error)
	ListFn   func() ([]os.DirEntry, error)
	GetFn    func(string) ([]byte, error)
	PutFn    func(string, []byte) error
	DeleteFn func(string) error
	CreateFn func(string) error
	RenameFn func(string, string) error
}

func (s *storeConfig) Stat(name string) (os.FileInfo, error) { return s.StatFn(name) }
func (s *storeConfig) List() ([]os.DirEntry, error)          { return s.ListFn() }
func (s *storeConfig) Get(name string) ([]byte, error)       { return s.GetFn(name) }
func (s *storeConfig) Put(name string, data []byte) error    { return s.PutFn(name, data) }
func (s *storeConfig) Delete(name string) error              { return s.DeleteFn(name) }
func (s *storeConfig) Create(name string) error              { return s.CreateFn(name) }
func (s *storeConfig) Rename(old, new string) error          { return s.RenameFn(old, new) }

// NewFlatDir returns a Store backed by a directory on the local filesystem.
func NewFlatDir(dir string, perm os.FileMode) Store {
	join := func(name string) string { return filepath.Join(dir, name) }
	ensureDir := func() error { return os.MkdirAll(dir, 0755) }

	return &storeConfig{
		StatFn: func(name string) (os.FileInfo, error) { return os.Stat(join(name)) },
		ListFn: func() ([]os.DirEntry, error) { return os.ReadDir(dir) },
		GetFn:  func(name string) ([]byte, error) { return os.ReadFile(join(name)) },
		PutFn: func(name string, data []byte) error {
			if err := ensureDir(); err != nil {
				return err
			}
			return os.WriteFile(join(name), data, perm)
		},
		DeleteFn: func(name string) error { return os.Remove(join(name)) },
		CreateFn: func(name string) error {
			if err := ensureDir(); err != nil {
				return err
			}
			return os.WriteFile(join(name), nil, perm)
		},
		RenameFn: func(old, new string) error { return os.Rename(join(old), join(new)) },
	}
}

// SyntheticFileInfo implements os.FileInfo for entries with no backing file.
type SyntheticFileInfo struct {
	Name_  string
	Mode_  os.FileMode
	Size_  int64
	IsDir_ bool
}

func (f *SyntheticFileInfo) Name() string       { return f.Name_ }
func (f *SyntheticFileInfo) Size() int64        { return f.Size_ }
func (f *SyntheticFileInfo) Mode() os.FileMode  { return f.Mode_ }
func (f *SyntheticFileInfo) ModTime() time.Time { return time.Time{} }
func (f *SyntheticFileInfo) IsDir() bool        { return f.IsDir_ }
func (f *SyntheticFileInfo) Sys() any           { return nil }

// SyntheticEntry implements os.DirEntry for entries with no backing file.
type SyntheticEntry struct {
	Name_  string
	Mode_  os.FileMode
	IsDir_ bool
}

func (e *SyntheticEntry) Name() string      { return e.Name_ }
func (e *SyntheticEntry) IsDir() bool       { return e.IsDir_ }
func (e *SyntheticEntry) Type() os.FileMode {
	if e.IsDir_ {
		return os.ModeDir
	}
	return 0
}
func (e *SyntheticEntry) Info() (os.FileInfo, error) {
	return &SyntheticFileInfo{Name_: e.Name_, Mode_: e.Mode_, IsDir_: e.IsDir_}, nil
}

// FileEntry returns a synthetic file DirEntry.
func FileEntry(name string, mode os.FileMode) os.DirEntry {
	return &SyntheticEntry{Name_: name, Mode_: mode}
}

// DirEntry returns a synthetic directory DirEntry.
func DirEntry(name string, mode os.FileMode) os.DirEntry {
	return &SyntheticEntry{Name_: name, Mode_: mode, IsDir_: true}
}
