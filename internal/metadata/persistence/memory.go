// internal/metadata/persistence/memory.go
package persistence

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"sync"

	"github.com/marmos91/dittofs/internal/metadata"
)

// MemoryRepository implements Repository using in-memory storage
type MemoryRepository struct {
	mu          sync.RWMutex
	exports     map[string]*exportData // map[path]*exportData
	files       map[string]*metadata.FileAttr
	parents     map[string]metadata.FileHandle
	children    map[string]map[string]metadata.FileHandle
	handleIndex uint64 // counter for generating unique handles
}

type exportData struct {
	Export     metadata.Export
	RootHandle metadata.FileHandle
}

// NewMemoryRepository creates a new in-memory repository
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		exports:     make(map[string]*exportData),
		files:       make(map[string]*metadata.FileAttr),
		parents:     make(map[string]metadata.FileHandle),
		children:    make(map[string]map[string]metadata.FileHandle),
		handleIndex: 0,
	}
}

// Helper to convert FileHandle to string key
func handleToKey(handle metadata.FileHandle) string {
	return string(handle)
}

// generateFileHandle creates a unique file handle
func (r *MemoryRepository) generateFileHandle(seed string) metadata.FileHandle {
	r.handleIndex++
	data := fmt.Sprintf("%s-%d", seed, r.handleIndex)
	hash := sha256.Sum256([]byte(data))
	return hash[:]
}

// Export operations

func (r *MemoryRepository) AddExport(path string, options metadata.ExportOptions, rootAttr *metadata.FileAttr) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if export already exists
	if _, exists := r.exports[path]; exists {
		return fmt.Errorf("export already exists: %s", path)
	}

	// Generate root handle
	rootHandle := r.generateFileHandle(path)
	key := handleToKey(rootHandle)

	// Store root attributes
	r.files[key] = rootAttr

	// Initialize empty children map for the root directory
	r.children[key] = make(map[string]metadata.FileHandle)

	// Store export data
	r.exports[path] = &exportData{
		Export: metadata.Export{
			Path:    path,
			Options: options,
		},
		RootHandle: rootHandle,
	}

	return nil
}

func (r *MemoryRepository) GetExports() ([]metadata.Export, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]metadata.Export, 0, len(r.exports))
	for _, ed := range r.exports {
		result = append(result, ed.Export)
	}
	return result, nil
}

func (r *MemoryRepository) FindExport(path string) (*metadata.Export, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ed, exists := r.exports[path]
	if !exists {
		return nil, fmt.Errorf("export not found: %s", path)
	}
	return &ed.Export, nil
}

func (r *MemoryRepository) GetRootHandle(exportPath string) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ed, exists := r.exports[exportPath]
	if !exists {
		return nil, fmt.Errorf("export not found: %s", exportPath)
	}
	return ed.RootHandle, nil
}

func (r *MemoryRepository) DeleteExport(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.exports[path]; !exists {
		return fmt.Errorf("export not found: %s", path)
	}

	delete(r.exports, path)
	return nil
}

// File operations

func (r *MemoryRepository) CreateFile(handle metadata.FileHandle, attr *metadata.FileAttr) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := handleToKey(handle)
	if _, exists := r.files[key]; exists {
		return fmt.Errorf("file already exists")
	}

	r.files[key] = attr

	// If it's a directory, initialize children map
	if attr.Type == metadata.FileTypeDirectory {
		r.children[key] = make(map[string]metadata.FileHandle)
	}

	return nil
}

func (r *MemoryRepository) GetFile(handle metadata.FileHandle) (*metadata.FileAttr, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := handleToKey(handle)
	attr, exists := r.files[key]
	if !exists {
		return nil, fmt.Errorf("file not found")
	}

	return attr, nil
}

func (r *MemoryRepository) UpdateFile(handle metadata.FileHandle, attr *metadata.FileAttr) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := handleToKey(handle)
	if _, exists := r.files[key]; !exists {
		return fmt.Errorf("file not found")
	}

	r.files[key] = attr
	return nil
}

func (r *MemoryRepository) DeleteFile(handle metadata.FileHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := handleToKey(handle)
	if _, exists := r.files[key]; !exists {
		return fmt.Errorf("file not found")
	}

	delete(r.files, key)
	return nil
}

// Directory hierarchy operations

func (r *MemoryRepository) SetParent(child metadata.FileHandle, parent metadata.FileHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.parents[handleToKey(child)] = parent
	return nil
}

func (r *MemoryRepository) GetParent(child metadata.FileHandle) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	parent, exists := r.parents[handleToKey(child)]
	if !exists {
		return nil, fmt.Errorf("parent not found")
	}

	return parent, nil
}

func (r *MemoryRepository) AddChild(parent metadata.FileHandle, name string, child metadata.FileHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		r.children[parentKey] = make(map[string]metadata.FileHandle)
	}

	if _, exists := r.children[parentKey][name]; exists {
		return fmt.Errorf("child already exists: %s", name)
	}

	r.children[parentKey][name] = child
	return nil
}

func (r *MemoryRepository) GetChild(parent metadata.FileHandle, name string) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		return nil, fmt.Errorf("parent has no children")
	}

	child, exists := r.children[parentKey][name]
	if !exists {
		return nil, fmt.Errorf("child not found: %s", name)
	}

	return child, nil
}

func (r *MemoryRepository) GetChildren(parent metadata.FileHandle) (map[string]metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	parentKey := handleToKey(parent)
	children := r.children[parentKey]
	if children == nil {
		return make(map[string]metadata.FileHandle), nil
	}

	// Return a copy to avoid concurrent access issues
	result := make(map[string]metadata.FileHandle, len(children))
	maps.Copy(result, children)

	return result, nil
}

func (r *MemoryRepository) DeleteChild(parent metadata.FileHandle, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		return fmt.Errorf("parent has no children")
	}

	if _, exists := r.children[parentKey][name]; !exists {
		return fmt.Errorf("child not found: %s", name)
	}

	delete(r.children[parentKey], name)
	return nil
}

// Helper method to add files and directories easily
func (r *MemoryRepository) AddFileToDirectory(parentHandle metadata.FileHandle, name string, attr *metadata.FileAttr) (metadata.FileHandle, error) {
	fileHandle := r.generateFileHandle(name)

	if err := r.CreateFile(fileHandle, attr); err != nil {
		return nil, err
	}

	if err := r.AddChild(parentHandle, name, fileHandle); err != nil {
		return nil, err
	}

	if err := r.SetParent(fileHandle, parentHandle); err != nil {
		return nil, err
	}

	return fileHandle, nil
}
