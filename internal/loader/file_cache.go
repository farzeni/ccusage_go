package loader

import (
	"crypto/sha256"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sdpower/ccusage-go/internal/types"
)

const fileCacheVersion = 2

// cachedEntry is a flat, gob-encodable snapshot of a parsed UsageEntry.
// It stores the two cache token counts explicitly so we don't need Raw.
type cachedEntry struct {
	ID                string
	Timestamp         time.Time
	DateKey           string
	ProjectPath       string
	Model             string
	InputTokens       int
	OutputTokens      int
	TotalTokens       int
	Cost              float64
	APICost           float64
	CacheCreateCost   float64
	CacheReadCost     float64
	SessionID         string
	SessionName       string
	BlockType         string
	CacheCreateTokens int
	CacheReadTokens   int
}

type fileCacheData struct {
	Version      int
	Entries      []cachedEntry
	SessionNames map[string]string
}

var (
	cacheDir     string
	cacheDirOnce sync.Once
)

func getCacheDir() string {
	cacheDirOnce.Do(func() {
		cacheDir = filepath.Join(os.TempDir(), "ccusage_file_cache")
		os.MkdirAll(cacheDir, 0700) //nolint:errcheck
	})
	return cacheDir
}

// cachePathForFile returns the gob file path for a given source + mtime pair.
func cachePathForFile(sourcePath string, mtime time.Time) string {
	h := sha256.Sum256([]byte(sourcePath))
	prefix := fmt.Sprintf("%x", h[:8])
	return filepath.Join(getCacheDir(), fmt.Sprintf("%s_%d.gob", prefix, mtime.UnixNano()))
}

func hashPrefixForPath(sourcePath string) string {
	h := sha256.Sum256([]byte(sourcePath))
	return fmt.Sprintf("%x", h[:8])
}

// loadFileCache returns cached entries for sourcePath if the cache is fresh (mtime matches).
func loadFileCache(sourcePath string, mtime time.Time) ([]types.UsageEntry, map[string]string, bool) {
	f, err := os.Open(cachePathForFile(sourcePath, mtime))
	if err != nil {
		return nil, nil, false
	}
	defer f.Close()

	var data fileCacheData
	if err := gob.NewDecoder(f).Decode(&data); err != nil || data.Version != fileCacheVersion {
		return nil, nil, false
	}

	entries := make([]types.UsageEntry, len(data.Entries))
	for i, ce := range data.Entries {
		entries[i] = types.UsageEntry{
			ID:              ce.ID,
			Timestamp:       ce.Timestamp,
			DateKey:         ce.DateKey,
			ProjectPath:     ce.ProjectPath,
			Model:           ce.Model,
			InputTokens:     ce.InputTokens,
			OutputTokens:    ce.OutputTokens,
			TotalTokens:     ce.TotalTokens,
			Cost:            ce.Cost,
			APICost:         ce.APICost,
			CacheCreateCost: ce.CacheCreateCost,
			CacheReadCost:   ce.CacheReadCost,
			SessionID:       ce.SessionID,
			SessionName:     ce.SessionName,
			BlockType:       ce.BlockType,
			SourceFile:      sourcePath,
		}
		// Restore the minimal Raw map needed by calculator and session reporter.
		if ce.CacheCreateTokens > 0 || ce.CacheReadTokens > 0 {
			entries[i].Raw = map[string]interface{}{
				"cache_creation_input_tokens": ce.CacheCreateTokens,
				"cache_read_input_tokens":     ce.CacheReadTokens,
			}
		}
	}

	return entries, data.SessionNames, true
}

// saveFileCache persists entries to disk and removes any stale cache files for the same source.
func saveFileCache(sourcePath string, mtime time.Time, entries []types.UsageEntry, sessionNames map[string]string) {
	dir := getCacheDir()
	newPath := cachePathForFile(sourcePath, mtime)
	prefix := hashPrefixForPath(sourcePath)

	// Remove stale cache files for this source file (same prefix, different mtime).
	if des, err := os.ReadDir(dir); err == nil {
		for _, de := range des {
			n := de.Name()
			if strings.HasPrefix(n, prefix+"_") && filepath.Join(dir, n) != newPath {
				os.Remove(filepath.Join(dir, n)) //nolint:errcheck
			}
		}
	}

	ces := make([]cachedEntry, len(entries))
	for i, e := range entries {
		ce := cachedEntry{
			ID:              e.ID,
			Timestamp:       e.Timestamp,
			DateKey:         e.DateKey,
			ProjectPath:     e.ProjectPath,
			Model:           e.Model,
			InputTokens:     e.InputTokens,
			OutputTokens:    e.OutputTokens,
			TotalTokens:     e.TotalTokens,
			Cost:            e.Cost,
			APICost:         e.APICost,
			CacheCreateCost: e.CacheCreateCost,
			CacheReadCost:   e.CacheReadCost,
			SessionID:       e.SessionID,
			SessionName:     e.SessionName,
			BlockType:       e.BlockType,
		}
		if e.Raw != nil {
			if v, ok := e.Raw["cache_creation_input_tokens"].(int); ok {
				ce.CacheCreateTokens = v
			}
			if v, ok := e.Raw["cache_read_input_tokens"].(int); ok {
				ce.CacheReadTokens = v
			}
		}
		ces[i] = ce
	}

	f, err := os.Create(newPath)
	if err != nil {
		return
	}
	defer f.Close()
	gob.NewEncoder(f).Encode(fileCacheData{ //nolint:errcheck
		Version:      fileCacheVersion,
		Entries:      ces,
		SessionNames: sessionNames,
	})
}
