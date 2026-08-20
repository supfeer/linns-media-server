package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var videoMIMETypes = map[string]string{
	".3gp": "video/3gpp", ".avi": "video/x-msvideo", ".m2ts": "video/mp2t",
	".m4v": "video/mp4", ".mkv": "video/x-matroska", ".mov": "video/quicktime",
	".mp4": "video/mp4", ".mpeg": "video/mpeg", ".mpg": "video/mpeg",
	".ts": "video/mp2t", ".webm": "video/webm",
}

type librarySource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type mediaEntry struct {
	Rel       string
	Name      string
	Dir       bool
	Size      int64
	ModUnix   int64
	Root      string
	SourceRel string
}

type libraryChange struct {
	SystemID   uint32
	Containers map[string]uint32
}

type library struct {
	sources          []librarySource
	virtualRoot      bool
	mu               sync.RWMutex
	entries          map[string]mediaEntry
	updateID         uint32
	containerUpdates map[string]uint32
}

func newLibrary(root string) (*library, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve media directory: %w", err)
	}
	return newLibrarySet([]librarySource{{ID: stableUUID(abs), Name: filepath.Base(abs), Path: abs}}, false)
}

func newCatalog(sources []librarySource) (*library, error) {
	return newLibrarySet(sources, true)
}

func newLibrarySet(sources []librarySource, virtualRoot bool) (*library, error) {
	if len(sources) == 0 {
		return nil, errors.New("at least one library is required")
	}
	normalized := make([]librarySource, 0, len(sources))
	seen := make(map[string]bool)
	for _, source := range sources {
		abs, err := filepath.Abs(source.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve library path: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("open library %q: %w", source.Name, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("library path is not a directory: %s", abs)
		}
		if source.ID == "" {
			source.ID = stableUUID(abs)
		}
		if source.Name == "" {
			source.Name = filepath.Base(abs)
		}
		if seen[source.ID] {
			return nil, fmt.Errorf("duplicate library id: %s", source.ID)
		}
		seen[source.ID] = true
		source.Path = abs
		normalized = append(normalized, source)
	}
	entries, err := scanSources(normalized, virtualRoot)
	if err != nil {
		return nil, err
	}
	containerUpdates := map[string]uint32{".": 1}
	if virtualRoot {
		for _, source := range normalized {
			containerUpdates[source.ID] = 1
		}
	}
	return &library{sources: normalized, virtualRoot: virtualRoot, entries: entries, updateID: 1, containerUpdates: containerUpdates}, nil
}

func scanLibrary(root string) (map[string]mediaEntry, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return scanSources([]librarySource{{ID: stableUUID(abs), Name: filepath.Base(abs), Path: abs}}, false)
}

func scanSources(sources []librarySource, virtualRoot bool) (map[string]mediaEntry, error) {
	entries := make(map[string]mediaEntry)
	if virtualRoot {
		entries["."] = mediaEntry{Rel: ".", Name: "Libraries", Dir: true}
	}
	for _, source := range sources {
		rootInfo, err := os.Stat(source.Path)
		if err != nil {
			return nil, fmt.Errorf("scan library %q: %w", source.Name, err)
		}
		if virtualRoot {
			entries[source.ID] = mediaEntry{Rel: source.ID, Name: source.Name, Dir: true, ModUnix: rootInfo.ModTime().UnixNano(), Root: source.Path, SourceRel: "."}
		}
		err = filepath.WalkDir(source.Path, func(filePath string, dirEntry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if filePath == source.Path {
					return walkErr
				}
				slog.Warn("media path skipped", "path", filePath, "error", walkErr)
				if dirEntry != nil && dirEntry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(source.Path, filePath)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == "." && virtualRoot {
				return nil
			}
			if rel != "." && strings.HasPrefix(dirEntry.Name(), ".") {
				if dirEntry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if dirEntry.Type()&os.ModeSymlink != 0 {
				if dirEntry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := dirEntry.Info()
			if err != nil {
				if dirEntry.IsDir() {
					return filepath.SkipDir
				}
				slog.Warn("media entry skipped", "path", filePath, "error", err)
				return nil
			}
			if !info.IsDir() && (!info.Mode().IsRegular() || !isVideo(rel)) {
				return nil
			}
			entryRel := rel
			if virtualRoot {
				entryRel = path.Join(source.ID, rel)
			}
			entries[entryRel] = mediaEntry{Rel: entryRel, Name: info.Name(), Dir: info.IsDir(), Size: info.Size(), ModUnix: info.ModTime().UnixNano(), Root: source.Path, SourceRel: rel}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan library %q: %w", source.Name, err)
		}
	}
	return entries, nil
}

func isVideo(name string) bool {
	_, ok := videoMIMETypes[strings.ToLower(filepath.Ext(name))]
	return ok
}

func mimeType(name string) string {
	return videoMIMETypes[strings.ToLower(filepath.Ext(name))]
}

func sameEntries(left, right map[string]mediaEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if other, ok := right[key]; !ok || other != value {
			return false
		}
	}
	return true
}

func (library *library) watch(ctx context.Context, interval time.Duration, onChange func(libraryChange)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var pending map[string]mediaEntry
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next, err := scanSources(library.sources, library.virtualRoot)
		if err != nil {
			slog.Warn("media rescan failed", "error", err)
			pending = nil
			continue
		}
		if library.matches(next) {
			pending = nil
			continue
		}
		if pending == nil || !sameEntries(pending, next) {
			pending = next
			continue
		}
		change := library.apply(next)
		pending = nil
		slog.Info("media library updated", "system_update_id", change.SystemID)
		onChange(change)
	}
}

func (library *library) matches(entries map[string]mediaEntry) bool {
	library.mu.RLock()
	defer library.mu.RUnlock()
	return sameEntries(library.entries, entries)
}

func (library *library) apply(entries map[string]mediaEntry) libraryChange {
	library.mu.Lock()
	defer library.mu.Unlock()
	changedContainers := make(map[string]struct{})
	for rel, oldEntry := range library.entries {
		if newEntry, ok := entries[rel]; !ok || newEntry != oldEntry {
			changedContainers[existingParent(rel, entries)] = struct{}{}
		}
	}
	for rel, newEntry := range entries {
		if oldEntry, ok := library.entries[rel]; !ok || oldEntry != newEntry {
			changedContainers[existingParent(rel, entries)] = struct{}{}
		}
	}
	if library.updateID == ^uint32(0) {
		library.updateID = 1
	} else {
		library.updateID++
	}
	containerVersions := make(map[string]uint32, len(changedContainers))
	for rel := range changedContainers {
		library.containerUpdates[rel]++
		if library.containerUpdates[rel] == 0 {
			library.containerUpdates[rel] = 1
		}
		containerVersions[rel] = library.containerUpdates[rel]
	}
	library.entries = entries
	return libraryChange{SystemID: library.updateID, Containers: containerVersions}
}

func existingParent(rel string, entries map[string]mediaEntry) string {
	parent := parentRel(rel)
	for parent != "." {
		if entry, ok := entries[parent]; ok && entry.Dir {
			return parent
		}
		parent = parentRel(parent)
	}
	return "."
}

func parentRel(rel string) string {
	if rel == "." {
		return "."
	}
	parent := path.Dir(rel)
	if parent == "" || parent == "/" {
		return "."
	}
	return parent
}

func (library *library) entry(rel string) (mediaEntry, bool) {
	library.mu.RLock()
	defer library.mu.RUnlock()
	entry, ok := library.entries[rel]
	return entry, ok
}

func (library *library) children(rel string) ([]mediaEntry, bool) {
	library.mu.RLock()
	defer library.mu.RUnlock()
	parent, ok := library.entries[rel]
	if !ok || !parent.Dir {
		return nil, false
	}
	children := make([]mediaEntry, 0)
	for childRel, child := range library.entries {
		if childRel != rel && parentRel(childRel) == rel {
			children = append(children, child)
		}
	}
	sort.Slice(children, func(i, j int) bool {
		if children[i].Dir != children[j].Dir {
			return children[i].Dir
		}
		return strings.ToLower(children[i].Name) < strings.ToLower(children[j].Name)
	})
	return children, true
}

func (library *library) childCount(rel string) int {
	children, _ := library.children(rel)
	return len(children)
}

func (library *library) systemUpdateID() uint32 {
	library.mu.RLock()
	defer library.mu.RUnlock()
	return library.updateID
}

func (library *library) currentChange() libraryChange {
	library.mu.RLock()
	defer library.mu.RUnlock()
	return libraryChange{SystemID: library.updateID, Containers: map[string]uint32{}}
}

func idFor(rel string) string {
	if rel == "." {
		return "0"
	}
	return "p-" + base64.RawURLEncoding.EncodeToString([]byte(rel))
}

func relForID(id string) (string, error) {
	if id == "0" {
		return ".", nil
	}
	if !strings.HasPrefix(id, "p-") {
		return "", errors.New("invalid object id")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, "p-"))
	if err != nil {
		return "", errors.New("invalid object id")
	}
	rel := string(decoded)
	if !fs.ValidPath(rel) || rel == "." {
		return "", errors.New("invalid object id")
	}
	return rel, nil
}

func (library *library) openMedia(id string) (*os.File, mediaEntry, error) {
	rel, err := relForID(id)
	if err != nil {
		return nil, mediaEntry{}, err
	}
	entry, ok := library.entry(rel)
	if !ok || entry.Dir || entry.Root == "" {
		return nil, mediaEntry{}, os.ErrNotExist
	}
	filePath, err := securePath(entry.Root, entry.SourceRel)
	if err != nil {
		return nil, mediaEntry{}, err
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, mediaEntry{}, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err != nil {
			return nil, mediaEntry{}, err
		}
		return nil, mediaEntry{}, errors.New("media entry is not a regular file")
	}
	return file, entry, nil
}

func securePath(root, rel string) (string, error) {
	current := root
	for _, segment := range strings.Split(rel, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("invalid media path")
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symbolic links are not served")
		}
	}
	return current, nil
}
