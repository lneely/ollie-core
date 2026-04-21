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

// FlatDir implements Store backed by a directory on the local filesystem.
type FlatDir struct {
	dir  string
	perm os.FileMode
}

// NewFlatDir returns a FlatDir rooted at dir.
// perm is applied to newly created or written files.
func NewFlatDir(dir string, perm os.FileMode) *FlatDir {
	return &FlatDir{dir: dir, perm: perm}
}

func (s *FlatDir) Stat(name string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(s.dir, name))
}

func (s *FlatDir) List() ([]os.DirEntry, error) {
	return os.ReadDir(s.dir)
}

func (s *FlatDir) Get(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.dir, name))
}

func (s *FlatDir) Put(name string, data []byte) error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, name), data, s.perm)
}

func (s *FlatDir) Delete(name string) error {
	return os.Remove(filepath.Join(s.dir, name))
}

func (s *FlatDir) Create(name string) error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, name), nil, s.perm)
}

func (s *FlatDir) Rename(oldName, newName string) error {
	return os.Rename(filepath.Join(s.dir, oldName), filepath.Join(s.dir, newName))
}
