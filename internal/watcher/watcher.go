// Package watcher предоставляет рекурсивный fs-вотчер с дебаунсом и фильтрацией.
package watcher

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aitools/internal/fileutil"

	"github.com/fsnotify/fsnotify"
)

// EventKind описывает тип изменения.
type EventKind int

const (
	EventCreate EventKind = iota + 1
	EventWrite
	EventRemove
	EventRename
)

func (k EventKind) String() string {
	switch k {
	case EventCreate:
		return "create"
	case EventWrite:
		return "write"
	case EventRemove:
		return "remove"
	case EventRename:
		return "rename"
	}
	return "?"
}

// Event — событие об изменении файла (после дебаунса).
type Event struct {
	Kind    EventKind
	AbsPath string
	RelPath string
	Time    time.Time
}

// Watcher рекурсивно следит за директорией, фильтруя по matcher + extensions,
// и публикует дебаунсенные события.
type Watcher struct {
	root       string
	matcher    *fileutil.Matcher
	extensions []string
	debounce   time.Duration

	fs *fsnotify.Watcher

	mu      sync.Mutex
	pending map[string]Event
	timer   *time.Timer

	out chan Event
}

// New создаёт вотчер. Не запускает обход — это делает Start.
func New(root string, matcher *fileutil.Matcher, extensions []string, debounce time.Duration) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if debounce <= 0 {
		debounce = 250 * time.Millisecond
	}
	return &Watcher{
		root:       root,
		matcher:    matcher,
		extensions: extensions,
		debounce:   debounce,
		fs:         fw,
		pending:    make(map[string]Event),
		out:        make(chan Event, 256),
	}, nil
}

// Events возвращает канал событий (дебаунсенных).
func (w *Watcher) Events() <-chan Event { return w.out }

// Start добавляет в подписку все вложенные директории (с учётом ignore) и запускает цикл.
func (w *Watcher) Start(ctx context.Context) error {
	if err := w.addDirRecursively(w.root); err != nil {
		return err
	}
	go w.loop(ctx)
	return nil
}

// Close — освобождает ресурсы.
func (w *Watcher) Close() error {
	return w.fs.Close()
}

func (w *Watcher) addDirRecursively(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(w.root, path)
		if w.matcher != nil && w.matcher.IsIgnored(rel, true) {
			return filepath.SkipDir
		}
		_ = w.fs.Add(path)
		return nil
	})
}

func (w *Watcher) shouldAccept(path string) bool {
	rel, err := filepath.Rel(w.root, path)
	if err != nil {
		return false
	}
	if w.matcher != nil && w.matcher.IsIgnored(rel, false) {
		return false
	}
	if len(w.extensions) > 0 {
		ext := strings.ToLower(filepath.Ext(path))
		ok := false
		for _, e := range w.extensions {
			if strings.ToLower(e) == ext {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (w *Watcher) loop(ctx context.Context) {
	defer close(w.out)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.fs.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case _, ok := <-w.fs.Errors:
			if !ok {
				return
			}
			// игнорируем — не критично для индексации
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	// если создана новая директория — подписываемся рекурсивно
	if ev.Op&fsnotify.Create != 0 {
		if info, err := fileInfo(ev.Name); err == nil && info != nil && info.IsDir() {
			rel, _ := filepath.Rel(w.root, ev.Name)
			if w.matcher == nil || !w.matcher.IsIgnored(rel, true) {
				_ = w.addDirRecursively(ev.Name)
			}
			return
		}
	}
	if !w.shouldAccept(ev.Name) {
		return
	}
	rel, _ := filepath.Rel(w.root, ev.Name)
	kind := EventWrite
	switch {
	case ev.Op&fsnotify.Create != 0:
		kind = EventCreate
	case ev.Op&fsnotify.Remove != 0:
		kind = EventRemove
	case ev.Op&fsnotify.Rename != 0:
		kind = EventRename
	case ev.Op&fsnotify.Write != 0:
		kind = EventWrite
	default:
		return
	}
	w.enqueue(Event{Kind: kind, AbsPath: ev.Name, RelPath: rel, Time: time.Now()})
}

func (w *Watcher) enqueue(e Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// последнее событие по файлу побеждает (свежее состояние важнее)
	w.pending[e.AbsPath] = e
	if w.timer == nil {
		w.timer = time.AfterFunc(w.debounce, w.flush)
	} else {
		w.timer.Reset(w.debounce)
	}
}

func (w *Watcher) flush() {
	w.mu.Lock()
	pending := w.pending
	w.pending = make(map[string]Event, len(pending))
	w.timer = nil
	w.mu.Unlock()
	for _, e := range pending {
		select {
		case w.out <- e:
		default:
			// канал переполнен — дропаем самые старые
		}
	}
}
