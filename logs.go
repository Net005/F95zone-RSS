package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type LogEntry struct {
	ID    int64     `json:"id"`
	Time  time.Time `json:"ts"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

// LogHub keeps a ring buffer of recent entries, appends to the log file and
// fans entries out to live subscribers (the SSE endpoint).
type LogHub struct {
	mu     sync.Mutex
	buf    []LogEntry
	cap    int
	nextID int64
	subs   map[chan LogEntry]struct{}
	file   *os.File
	path   string
}

func NewLogHub(path string) *LogHub {
	h := &LogHub{cap: 5000, subs: map[chan LogEntry]struct{}{}, path: path, nextID: 1}
	if st, err := os.Stat(path); err == nil && st.Size() > 20<<20 {
		_ = os.Rename(path, path+".1")
	}
	h.seedFromFile()
	h.file, _ = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	return h
}

// seedFromFile loads the tail of the existing log so the UI has history after a restart.
func (h *LogHub) seedFromFile() {
	f, err := os.Open(h.path)
	if err != nil {
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	const tail = 256 << 10
	if st.Size() > tail {
		f.Seek(st.Size()-tail, io.SeekStart)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	first := st.Size() > tail
	mod := st.ModTime()
	for sc.Scan() {
		if first { // partial line
			first = false
			continue
		}
		line := sc.Text()
		if len(line) < 14 || line[0] != '[' {
			continue
		}
		i := strings.Index(line, "] ")
		j := strings.Index(line, " | ")
		if i < 0 || j < i {
			continue
		}
		t, err := time.ParseInLocation("15:04:05", line[1:i], time.Local)
		if err != nil {
			continue
		}
		ts := time.Date(mod.Year(), mod.Month(), mod.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local)
		if ts.After(mod.Add(time.Minute)) {
			ts = ts.AddDate(0, 0, -1)
		}
		lvl := strings.TrimSpace(line[i+2 : j])
		if lvl == "WARNING" {
			lvl = "WARN"
		}
		h.appendLocked(LogEntry{Time: ts, Level: lvl, Msg: line[j+3:]}, false)
	}
}

func (h *LogHub) appendLocked(e LogEntry, fanout bool) LogEntry {
	e.ID = h.nextID
	h.nextID++
	h.buf = append(h.buf, e)
	if len(h.buf) > h.cap {
		h.buf = h.buf[len(h.buf)-h.cap:]
	}
	if fanout {
		for ch := range h.subs {
			select {
			case ch <- e:
			default:
			}
		}
	}
	return e
}

func (h *LogHub) Log(level, format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	now := time.Now()
	h.mu.Lock()
	e := h.appendLocked(LogEntry{Time: now, Level: level, Msg: msg}, true)
	if h.file != nil {
		fmt.Fprintf(h.file, "[%s] %-5s | %s\n", now.Format("15:04:05"), level, msg)
	}
	h.mu.Unlock()
	fmt.Printf("[%s] %-5s | %s\n", e.Time.Format("15:04:05"), level, msg)
}

func (h *LogHub) Debug(f string, a ...any) { h.Log("DEBUG", f, a...) }
func (h *LogHub) Info(f string, a ...any)  { h.Log("INFO", f, a...) }
func (h *LogHub) Warn(f string, a ...any)  { h.Log("WARN", f, a...) }
func (h *LogHub) Error(f string, a ...any) { h.Log("ERROR", f, a...) }

// Since returns entries with ID > id (at most limit, newest ones).
func (h *LogHub) Since(id int64, limit int) []LogEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []LogEntry
	for _, e := range h.buf {
		if e.ID > id {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (h *LogHub) Subscribe() (chan LogEntry, func()) {
	ch := make(chan LogEntry, 1024)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Clear empties the in-memory buffer and truncates the log file.
func (h *LogHub) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf = nil
	if h.file != nil {
		h.file.Truncate(0)
		h.file.Seek(0, io.SeekStart)
	}
}

func (h *LogHub) Path() string { return h.path }
