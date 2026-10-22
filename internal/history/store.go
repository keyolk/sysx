package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Store is a directory of one JSONL file per local day: 2026-10-22.jsonl.
// Day files make retention a file delete, and a reader of the last hours
// opens one or two files.
type Store struct {
	Dir string
	// Keep is how many days of files to keep; MaxBytes caps the total,
	// dropping the oldest days first. Zero disables each.
	Keep     int
	MaxBytes int64
}

// DefaultDir is $XDG_STATE_HOME/sysx/history.
func DefaultDir() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "sysx", "history")
}

const dayLayout = "2006-01-02"

func (s Store) file(day time.Time) string {
	return filepath.Join(s.Dir, day.Local().Format(dayLayout)+".jsonl")
}

// Append writes episodes to the file of the day each one ended.
func (s Store) Append(eps []Episode) error {
	if len(eps) == 0 {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	byFile := map[string][]Episode{}
	for _, e := range eps {
		f := s.file(e.End)
		byFile[f] = append(byFile[f], e)
	}
	var errs []error
	for path, list := range byFile {
		if err := appendFile(path, list); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func appendFile(path string, eps []Episode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range eps {
		if err := enc.Encode(e); err != nil {
			f.Close()
			return fmt.Errorf("encode %s: %w", e.Key, err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

type dayFile struct {
	path string
	day  time.Time
	size int64
}

func (s Store) days() ([]dayFile, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []dayFile
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || e.IsDir() {
			continue
		}
		day, err := time.ParseInLocation(dayLayout, name, time.Local)
		if err != nil {
			continue // not ours
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, dayFile{filepath.Join(s.Dir, e.Name()), day, info.Size()})
	}
	slices.SortFunc(out, func(a, b dayFile) int { return a.day.Compare(b.day) })
	return out, nil
}

// Prune deletes day files older than Keep days, then the oldest days until
// the total fits MaxBytes. Today's file is never deleted. It returns the
// files it removed.
func (s Store) Prune(now time.Time) ([]string, error) {
	days, err := s.days()
	if err != nil {
		return nil, err
	}
	today := now.Local().Format(dayLayout)
	var removed []string
	var errs []error
	rm := func(d dayFile) bool {
		if filepath.Base(d.path) == today+".jsonl" {
			return false
		}
		if err := os.Remove(d.path); err != nil {
			errs = append(errs, err)
			return false
		}
		removed = append(removed, d.path)
		return true
	}
	var kept []dayFile
	cut := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -(s.Keep - 1))
	for _, d := range days {
		if s.Keep > 0 && d.day.Before(cut) && rm(d) {
			continue
		}
		kept = append(kept, d)
	}
	if s.MaxBytes > 0 {
		var total int64
		for _, d := range kept {
			total += d.size
		}
		for _, d := range kept {
			if total <= s.MaxBytes {
				break
			}
			if rm(d) {
				total -= d.size
			}
		}
	}
	return removed, errors.Join(errs...)
}

// Load reads episodes that ended at or after since, joins segments and
// returns them newest first. A torn last line (the recorder was writing)
// is skipped, not an error.
func (s Store) Load(since time.Time) ([]Episode, error) {
	days, err := s.days()
	if err != nil {
		return nil, err
	}
	first := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.Local)
	var eps []Episode
	var errs []error
	for _, d := range days {
		if d.day.Before(first) {
			continue
		}
		f, err := os.Open(d.path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var e Episode
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if !e.End.Before(since) {
				eps = append(eps, e)
			}
		}
		if err := sc.Err(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.path, err))
		}
		f.Close()
	}
	return Merge(eps, 30*time.Second), errors.Join(errs...)
}

// Size is the total bytes and day count on disk.
func (s Store) Size() (bytes int64, days int, err error) {
	ds, err := s.days()
	for _, d := range ds {
		bytes += d.size
	}
	return bytes, len(ds), err
}

// Lock is held by the one running recorder.
type Lock struct{ f *os.File }

func lockPath(dir string) string { return filepath.Join(dir, "record.lock") }

// TryLock takes the recorder lock and writes our pid into it. It fails when
// another recorder holds it.
func TryLock(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath(dir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		pid, _ := RecorderPID(dir)
		return nil, fmt.Errorf("another recorder is running (pid %d)", pid)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0) // the pid is informational
	}
	return &Lock{f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		l.f.Close()
	}
}

// RecorderPID reports the running recorder's pid, or 0 when none holds the
// lock. A stale pid in the file is not trusted: the lock is the truth.
func RecorderPID(dir string) (int, error) {
	f, err := os.Open(lockPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // closing the file below releases it anyway
		return 0, nil                               // nobody holds it
	}
	b := make([]byte, 16)
	n, _ := f.Read(b)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	return pid, nil
}
