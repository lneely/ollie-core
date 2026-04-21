// Package store defines storage interfaces and a filesystem-backed
// implementation used by the agent runtime and 9P server.
package store

import (
	"os"
	"path/filepath"
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
