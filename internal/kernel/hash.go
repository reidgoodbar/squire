package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

const maxFileHashCacheEntries = 8192

type fileHashCacheEntry struct {
	signal string
	hash   string
}

var fileHashCache = struct {
	sync.Mutex
	entries map[string]fileHashCacheEntry
}{entries: map[string]fileHashCacheEntry{}}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashString(s string) string {
	return hashBytes([]byte(s))
}

func hashFile(path string) (string, bool) {
	clean := filepath.Clean(path)
	info, err := os.Stat(clean)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return "", false
	}
	signal, cacheable := fileHashStatCacheSignal(info)
	if cacheable {
		fileHashCache.Lock()
		if cached, ok := fileHashCache.entries[clean]; ok && cached.signal == signal {
			fileHashCache.Unlock()
			return cached.hash, true
		}
		fileHashCache.Unlock()
	}

	f, err := os.Open(clean)
	if err != nil {
		return "", false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	after, err := f.Stat()
	afterSignal, _ := fileHashStatCacheSignal(after)
	if err != nil || afterSignal != signal {
		return "", false
	}
	hash := hex.EncodeToString(h.Sum(nil))
	if cacheable {
		fileHashCache.Lock()
		if len(fileHashCache.entries) >= maxFileHashCacheEntries {
			fileHashCache.entries = map[string]fileHashCacheEntry{}
		}
		fileHashCache.entries[clean] = fileHashCacheEntry{signal: signal, hash: hash}
		fileHashCache.Unlock()
	}
	return hash, true
}

func fileHashStatSignal(info os.FileInfo) string {
	signal, _ := fileHashStatCacheSignal(info)
	return signal
}

func fileHashStatCacheSignal(info os.FileInfo) (string, bool) {
	change, ok := fileStatChangeSignal(info)
	if !ok {
		change = "change:unsupported"
	}
	return strconv.FormatInt(info.Size(), 10) + "|" +
		strconv.FormatInt(info.ModTime().UnixNano(), 10) + "|" +
		info.Mode().String() + "|" +
		change, ok
}
