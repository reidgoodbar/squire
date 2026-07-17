package green

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type repositoryMonitor struct {
	root     string
	matchers []inputMatcher
	watcher  *fsnotify.Watcher
	changes  chan struct{}
	errors   chan error
	done     chan struct{}
	close    sync.Once
}

func startRepositoryMonitor(repoRoot string, checks []Check) (*repositoryMonitor, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	monitor := &repositoryMonitor{
		root:     filepath.Clean(repoRoot),
		watcher:  watcher,
		changes:  make(chan struct{}, 1),
		errors:   make(chan error, 1),
		done:     make(chan struct{}),
		matchers: make([]inputMatcher, 0, len(checks)),
	}
	for _, check := range checks {
		monitor.matchers = append(monitor.matchers, inputMatcher{include: check.Inputs, exclude: check.Exclude})
	}
	if err := monitor.addRelevantDirectories(monitor.root); err != nil {
		_ = watcher.Close()
		return nil, err
	}
	go monitor.consume()
	return monitor, nil
}

func (monitor *repositoryMonitor) addRelevantDirectories(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(monitor.root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			return filepath.SkipDir
		}
		if rel != "." && rel != ".squire" && !monitor.directoryRelevant(rel) {
			return filepath.SkipDir
		}
		if err := monitor.watcher.Add(path); err != nil {
			return fmt.Errorf("watch %s: %w", rel, err)
		}
		return nil
	})
}

func (monitor *repositoryMonitor) directoryRelevant(rel string) bool {
	for _, matcher := range monitor.matchers {
		if matcher.couldContain(rel) && !matcher.excludesSubtree(rel) {
			return true
		}
	}
	return false
}

func (monitor *repositoryMonitor) consume() {
	defer close(monitor.done)
	for {
		select {
		case event, ok := <-monitor.watcher.Events:
			if !ok {
				return
			}
			monitor.handleEvent(event)
		case err, ok := <-monitor.watcher.Errors:
			if !ok {
				return
			}
			select {
			case monitor.errors <- err:
			default:
			}
			monitor.signalChange()
		}
	}
}

func (monitor *repositoryMonitor) handleEvent(event fsnotify.Event) {
	rel, err := filepath.Rel(monitor.root, event.Name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		select {
		case monitor.errors <- fmt.Errorf("repository monitor escaped workspace"):
		default:
		}
		monitor.signalChange()
		return
	}
	rel = filepath.ToSlash(rel)
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return
	}
	relevant := rel == ConfigRelativePath
	for _, matcher := range monitor.matchers {
		if matcher.matches(rel) {
			relevant = true
			break
		}
	}
	if event.Op&fsnotify.Create != 0 {
		if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
			if rel == ".squire" || monitor.directoryRelevant(rel) {
				if err := monitor.addRelevantDirectories(event.Name); err != nil {
					select {
					case monitor.errors <- err:
					default:
					}
				}
				relevant = true
			}
		}
	}
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && monitor.directoryRelevant(rel) {
		relevant = true
	}
	if relevant {
		monitor.signalChange()
	}
}

func (monitor *repositoryMonitor) signalChange() {
	select {
	case monitor.changes <- struct{}{}:
	default:
	}
}

func (monitor *repositoryMonitor) Close() {
	if monitor == nil {
		return
	}
	monitor.close.Do(func() {
		_ = monitor.watcher.Close()
		<-monitor.done
	})
}
