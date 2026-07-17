package green

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fsnotify/fsnotify"
)

type mutationGuard struct {
	root    string
	matcher inputMatcher
	watcher *fsnotify.Watcher
	done    chan struct{}
	mu      sync.Mutex
	changed bool
	reason  string
}

func startMutationGuard(repoRoot string, check Check) (*mutationGuard, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	guard := &mutationGuard{
		root:    filepath.Clean(repoRoot),
		matcher: inputMatcher{include: check.Inputs, exclude: check.Exclude},
		watcher: watcher,
		done:    make(chan struct{}),
	}
	if err := guard.addRelevantDirectories(guard.root); err != nil {
		_ = watcher.Close()
		return nil, err
	}
	go guard.consume()
	return guard, nil
}

func (guard *mutationGuard) addRelevantDirectories(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(guard.root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			return filepath.SkipDir
		}
		if rel != "." && guard.matcher.excludesSubtree(rel) {
			return filepath.SkipDir
		}
		if rel != "." && rel != ".squire" && !guard.matcher.couldContain(rel) {
			return filepath.SkipDir
		}
		if rel == "." || guard.matcher.couldContain(rel) || rel == ".squire" {
			if err := guard.watcher.Add(path); err != nil {
				return fmt.Errorf("watch %s: %w", rel, err)
			}
		}
		return nil
	})
}

func (guard *mutationGuard) consume() {
	defer close(guard.done)
	for {
		select {
		case event, ok := <-guard.watcher.Events:
			if !ok {
				return
			}
			guard.handleEvent(event)
		case err, ok := <-guard.watcher.Errors:
			if !ok {
				return
			}
			guard.markChanged("watcher error: " + err.Error())
		}
	}
}

func (guard *mutationGuard) handleEvent(event fsnotify.Event) {
	if event.Op == fsnotify.Chmod {
		return
	}
	rel, err := filepath.Rel(guard.root, event.Name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		guard.markChanged("workspace watch escaped repository")
		return
	}
	rel = filepath.ToSlash(rel)
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return
	}
	if rel == ConfigRelativePath || guard.matcher.matches(rel) {
		guard.markChanged(fmt.Sprintf("%s changed (%s)", rel, event.Op.String()))
	}
	if event.Op&fsnotify.Create != 0 {
		if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
			if guard.matcher.couldContain(rel) && !guard.matcher.excludesSubtree(rel) {
				if err := guard.addRelevantDirectories(event.Name); err != nil {
					guard.markChanged("watch new directory: " + err.Error())
				}
				guard.markChanged(rel + " directory created")
			}
		}
	}
}

func (guard *mutationGuard) markChanged(reason string) {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	guard.changed = true
	if guard.reason == "" {
		guard.reason = reason
	}
}

func (guard *mutationGuard) Changed() (bool, string) {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return guard.changed, guard.reason
}

func (guard *mutationGuard) Close() {
	_ = guard.watcher.Close()
	<-guard.done
}

func (matcher inputMatcher) couldContain(dir string) bool {
	dir = strings.Trim(strings.TrimPrefix(filepath.ToSlash(dir), "./"), "/")
	for _, pattern := range matcher.include {
		prefix := literalPatternPrefix(pattern)
		if prefix == "" || dir == "" || dir == prefix || strings.HasPrefix(dir, prefix+"/") || strings.HasPrefix(prefix, dir+"/") {
			return true
		}
	}
	return false
}

func (matcher inputMatcher) excludesSubtree(dir string) bool {
	dir = strings.Trim(strings.TrimPrefix(filepath.ToSlash(dir), "./"), "/")
	for _, pattern := range matcher.exclude {
		if matched, _ := doublestar.Match(pattern, dir); matched {
			return true
		}
		if matched, _ := doublestar.Match(pattern, dir+"/__squire_probe__"); matched {
			return true
		}
	}
	return false
}

func literalPatternPrefix(pattern string) string {
	parts := strings.Split(filepath.ToSlash(pattern), "/")
	literal := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.ContainsAny(part, "*?[{") {
			break
		}
		literal = append(literal, part)
	}
	return strings.Trim(strings.Join(literal, "/"), "/")
}
