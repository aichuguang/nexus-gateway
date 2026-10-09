// Package traffic incrementally accounts for completed Nginx HTTP requests.
// Its ledger and source cursors commit together; no request details are retained.
package traffic

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"w103d/gateway/internal/atomicfile"
)

const (
	maxStateBytes = 8 << 20
	maxPollBytes  = 4 << 20
	maxLineBytes  = 4096
	maxFiles      = 512
	maxServices   = 2048
	prefixBytes   = 256
	tailBytes     = 64
)

var (
	shanghai    = time.FixedZone("Asia/Shanghai", 8*3600)
	serviceID   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	archiveName = regexp.MustCompile(`^traffic\.log-[0-9]{8}-[0-9]{6}(?:\.gz)?$`)
)

type Counters struct {
	Requests uint64 `json:"requests"`
	Received uint64 `json:"received"`
	Sent     uint64 `json:"sent"`
}
type ServiceStats struct {
	Total      Counters `json:"total"`
	Today      Counters `json:"today"`
	LastSeenAt string   `json:"lastSeenAt"`
}
type DayStats struct {
	Date     string              `json:"date"`
	Total    Counters            `json:"total"`
	Services map[string]Counters `json:"services"`
}
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Count   uint64 `json:"count"`
	LastAt  string `json:"lastAt"`
}
type Snapshot struct {
	StartedAt    string                  `json:"startedAt"`
	UpdatedAt    string                  `json:"updatedAt"`
	LastRecordAt string                  `json:"lastRecordAt"`
	Total        Counters                `json:"total"`
	Today        Counters                `json:"today"`
	Services     map[string]ServiceStats `json:"services"`
	Days         []DayStats              `json:"days"`
	Warnings     []Warning               `json:"warnings"`
	Incomplete   bool                    `json:"incomplete"`
	CatchingUp   bool                    `json:"catchingUp"`
}
type cursor struct {
	Identity   string `json:"identity"`
	Name       string `json:"name"`
	Offset     int64  `json:"offset"`
	Prefix     string `json:"prefix"`
	PrefixLen  int    `json:"prefixLen"`
	Tail       string `json:"tail"`
	Size       int64  `json:"size"`
	Compressed bool   `json:"compressed"`
	Done       bool   `json:"done"`
	Discard    bool   `json:"discard"`
	Blocked    bool   `json:"blocked"`
	Missing    bool   `json:"missing"`
	LastSeen   string `json:"lastSeen"`
}
type ledger struct {
	Version      int                     `json:"version"`
	StartedAt    string                  `json:"startedAt"`
	UpdatedAt    string                  `json:"updatedAt"`
	LastRecordAt string                  `json:"lastRecordAt"`
	Total        Counters                `json:"total"`
	Services     map[string]ServiceStats `json:"services"`
	Days         map[string]DayStats     `json:"days"`
	Files        map[string]*cursor      `json:"files"`
	Next         uint64                  `json:"next"`
	Warnings     []Warning               `json:"warnings"`
	Incomplete   bool                    `json:"incomplete"`
}
type source struct {
	name, identity string
	size           int64
	compressed     bool
	prefix         []byte
}

// A gzip recovery may need several bounded polls just to reach a persisted
// offset. The open reader is only an optimization; after restart it replays the
// skip, never the counters. It is discarded on an uncommitted ledger write.
type stream struct {
	key      string
	f        *os.File
	gz       *gzip.Reader
	r        *bufio.Reader
	position int64
	tail     []byte
	pending  []byte
	verified bool
}
type Collector struct {
	mu         sync.Mutex
	dir        string
	state      ledger
	active     *stream
	catchingUp bool
	now        func() time.Time
	write      func(string, []byte, atomicfile.Options) error
	budget     int
	persisted  bool
}

// New refuses corrupt state rather than silently replaying logs from zero.
// StartedAt is creation time even when traffic.log has not yet been created;
// Poll persists it. Existing retained archives may predate that timestamp.
func New(dataDir string) (*Collector, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, errors.New("traffic data directory must be absolute")
	}
	c := &Collector{dir: dataDir, now: time.Now, write: atomicfile.Write, budget: maxPollBytes}
	path := filepath.Join(dataDir, "traffic-state.json")
	f, err := safeOpen(path)
	if errors.Is(err, os.ErrNotExist) {
		c.state = ledger{Version: 1, StartedAt: stamp(c.now()), Services: map[string]ServiceStats{}, Days: map[string]DayStats{}, Files: map[string]*cursor{}, Warnings: []Warning{}}
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("traffic state cannot be opened safely: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil || len(data) > maxStateBytes {
		return nil, errors.New("traffic state is unreadable or exceeds its size limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&c.state); err != nil {
		return nil, fmt.Errorf("invalid traffic state: %w", err)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("invalid trailing traffic state")
	}
	if err = validate(c.state); err != nil {
		return nil, err
	}
	c.persisted = true
	return c, nil
}

func safeOpen(path string) (*os.File, error) {
	i, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !i.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := openSource(path)
	if err != nil {
		return nil, err
	}
	j, err := f.Stat()
	if err != nil || !j.Mode().IsRegular() || !os.SameFile(i, j) {
		f.Close()
		return nil, errors.New("file changed while opening")
	}
	return f, nil
}
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func hash(b []byte) string     { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func add(a, b Counters) (Counters, bool) {
	if math.MaxUint64-a.Requests < b.Requests || math.MaxUint64-a.Received < b.Received || math.MaxUint64-a.Sent < b.Sent {
		return a, false
	}
	return Counters{a.Requests + b.Requests, a.Received + b.Received, a.Sent + b.Sent}, true
}
func validStamp(s string) bool { _, e := time.Parse(time.RFC3339Nano, s); return e == nil }
func validHash(s string) bool  { b, e := hex.DecodeString(s); return e == nil && len(b) == 32 }
func validate(s ledger) error {
	bad := func() error { return errors.New("traffic state is invalid; refusing to reset accounting") }
	if s.Version != 1 || !validStamp(s.StartedAt) || (s.UpdatedAt != "" && !validStamp(s.UpdatedAt)) || (s.LastRecordAt != "" && !validStamp(s.LastRecordAt)) || s.Services == nil || s.Days == nil || s.Files == nil || len(s.Services) > maxServices || len(s.Files) > maxFiles || len(s.Days) > 30 || len(s.Warnings) > 32 {
		return bad()
	}
	var total Counters
	for id, v := range s.Services {
		if !serviceID.MatchString(id) || v.Today != (Counters{}) || (v.LastSeenAt != "" && !validStamp(v.LastSeenAt)) {
			return bad()
		}
		var ok bool
		total, ok = add(total, v.Total)
		if !ok {
			return bad()
		}
	}
	if total != s.Total {
		return bad()
	}
	retained := map[string]Counters{}
	for date, day := range s.Days {
		if _, e := time.Parse("2006-01-02", date); e != nil || date != day.Date || day.Services == nil || len(day.Services) > maxServices {
			return bad()
		}
		var sum Counters
		for id, v := range day.Services {
			if _, ok := s.Services[id]; !ok {
				return bad()
			}
			var ok bool
			sum, ok = add(sum, v)
			if !ok {
				return bad()
			}
			retained[id], ok = add(retained[id], v)
			all := s.Services[id].Total
			if !ok || retained[id].Requests > all.Requests || retained[id].Received > all.Received || retained[id].Sent > all.Sent {
				return bad()
			}
		}
		if sum != day.Total {
			return bad()
		}
	}
	identities := map[string]bool{}
	for key, f := range s.Files {
		n, e := strconv.ParseUint(key, 10, 64)
		if e != nil || n == 0 || n > s.Next || f == nil || f.Identity == "" || len(f.Identity) > 160 || !logName(f.Name) || f.Offset < 0 || f.Size < 0 || f.PrefixLen < 0 || f.PrefixLen > prefixBytes || int64(f.PrefixLen) > f.Offset || !validStamp(f.LastSeen) || (f.Offset > 0 && (!validHash(f.Prefix) || !validHash(f.Tail))) {
			return bad()
		}
		if identities[f.Identity] || f.Compressed != strings.HasSuffix(f.Name, ".gz") {
			return bad()
		}
		identities[f.Identity] = true
	}
	seen := map[string]bool{}
	for _, w := range s.Warnings {
		if w.Code == "" || len(w.Code) > 64 || seen[w.Code] || len(w.Message) > 300 || !validStamp(w.LastAt) || w.Count == 0 {
			return bad()
		}
		seen[w.Code] = true
	}
	return nil
}
func logName(s string) bool { return s == "traffic.log" || archiveName.MatchString(s) }
func warn(s *ledger, now time.Time, code, message string, incomplete bool) {
	s.Incomplete = s.Incomplete || incomplete
	for i := range s.Warnings {
		if s.Warnings[i].Code == code {
			if s.Warnings[i].Count < math.MaxUint64 {
				s.Warnings[i].Count++
			}
			s.Warnings[i].LastAt = stamp(now)
			return
		}
	}
	if len(s.Warnings) < 32 {
		s.Warnings = append(s.Warnings, Warning{code, message, 1, stamp(now)})
	}
}

func (c *Collector) closeStream() {
	if c.active != nil {
		if c.active.gz != nil {
			c.active.gz.Close()
		}
		c.active.f.Close()
		c.active = nil
	}
}

// Close releases an in-progress decompression reader; committed counters remain.
func (c *Collector) Close() error { c.mu.Lock(); defer c.mu.Unlock(); c.closeStream(); return nil }

func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state
	today := c.now().In(shanghai).Format("2006-01-02")
	out := Snapshot{StartedAt: s.StartedAt, UpdatedAt: s.UpdatedAt, LastRecordAt: s.LastRecordAt, Total: s.Total, Services: map[string]ServiceStats{}, Days: []DayStats{}, Warnings: append([]Warning{}, s.Warnings...), Incomplete: s.Incomplete, CatchingUp: c.catchingUp}
	for id, v := range s.Services {
		v.Today = s.Days[today].Services[id]
		out.Services[id] = v
	}
	for i := 29; i >= 0; i-- {
		date := c.now().In(shanghai).AddDate(0, 0, -i).Format("2006-01-02")
		d := s.Days[date]
		copy := DayStats{Date: date, Total: d.Total, Services: map[string]Counters{}}
		for id, v := range d.Services {
			copy.Services[id] = v
		}
		out.Days = append(out.Days, copy)
	}
	out.Today = s.Days[today].Total
	return out
}

func (c *Collector) sources(ctx context.Context, s *ledger, now time.Time) ([]source, error) {
	d, err := os.Open(c.dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var names []string
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, e := d.ReadDir(64)
		entries += len(batch)
		if entries > 8192 {
			warn(s, now, "directory_limit", "Traffic directory scan limit reached; some files were not inspected.", true)
			break
		}
		for _, item := range batch {
			if logName(item.Name()) {
				names = append(names, item.Name())
				if len(names) > maxFiles {
					return nil, errors.New("traffic log file limit exceeded")
				}
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "traffic.log" {
			return false
		}
		if names[j] == "traffic.log" {
			return true
		}
		return names[i] < names[j]
	})
	named := make(map[string]bool, len(names))
	for _, name := range names {
		named[name] = true
	}
	out := []source{}
	for _, name := range names {
		// During gzip, logrotate keeps the plain archive until compression has
		// finished. Never inspect or replay the concurrently written .gz file.
		if strings.HasSuffix(name, ".gz") && named[strings.TrimSuffix(name, ".gz")] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := safeOpen(filepath.Join(c.dir, name))
		if err != nil {
			warn(s, now, "source_unreadable", "A traffic source is missing, unsafe, or unreadable; collection may be incomplete.", true)
			continue
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		id, err := fileIdentity(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		item := source{name: name, identity: id, size: info.Size(), compressed: strings.HasSuffix(name, ".gz")}
		var r io.Reader = f
		var g *gzip.Reader
		if item.compressed {
			g, err = gzip.NewReader(f)
			if err != nil {
				f.Close()
				warn(s, now, "gzip_invalid", "A compressed traffic archive cannot be decoded; accounting is incomplete.", true)
				continue
			}
			r = g
		}
		item.prefix = make([]byte, prefixBytes)
		n, e := io.ReadFull(r, item.prefix)
		item.prefix = item.prefix[:n]
		if g != nil {
			g.Close()
		}
		f.Close()
		if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
			warn(s, now, "source_read", "A traffic archive could not be read completely.", true)
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func prefixMatches(f *cursor, src source) bool {
	return f.PrefixLen == 0 || len(src.prefix) >= f.PrefixLen && hash(src.prefix[:f.PrefixLen]) == f.Prefix
}

func stillPresent(f *cursor, sources []source) bool {
	for _, src := range sources {
		if src.identity == f.Identity && src.compressed == f.Compressed && prefixMatches(f, src) {
			return true
		}
	}
	return false
}

// Poll performs a bounded pass, then commits aggregates and cursors atomically.
// Cancellation or a pre-commit write failure leaves both at their old values.
func (c *Collector) Poll(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	data, _ := json.Marshal(c.state)
	previousBytes := data
	var next ledger
	if err := json.Unmarshal(data, &next); err != nil {
		return err
	}
	now := c.now()
	oldest := now.In(shanghai).AddDate(0, 0, -29).Format("2006-01-02")
	today := now.In(shanghai).Format("2006-01-02")
	for date := range next.Days {
		if date < oldest || date > today {
			delete(next.Days, date)
		}
	}
	sources, err := c.sources(ctx, &next, now)
	if err != nil {
		c.closeStream()
		return err
	}
	seen := map[string]bool{}
	remaining := c.budget
	catching := false
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			c.closeStream()
			return err
		}
		var key string
		var cur *cursor
		for k, f := range next.Files {
			if f.Identity == src.identity && f.Compressed == src.compressed {
				key, cur = k, f
				break
			}
		}
		if cur == nil && src.compressed {
			// logrotate replaces the inode on gzip compression. Only associate an
			// absent old inode, and verify its boundary while replaying the skip.
			matches := []string{}
			for k, f := range next.Files {
				if !stillPresent(f, sources) && !f.Compressed && !f.Blocked && prefixMatches(f, src) && (strings.TrimSuffix(src.name, ".gz") == f.Name || f.PrefixLen > 0) {
					matches = append(matches, k)
				}
			}
			if len(matches) > 1 {
				for _, k := range matches {
					seen[k] = true
				}
				warn(&next, now, "gzip_ambiguous", "A compressed archive cannot be uniquely linked to prior accounting; it was not replayed.", true)
				continue
			}
			if len(matches) == 1 {
				key = matches[0]
				cur = next.Files[key]
				cur.Identity = src.identity
				cur.Compressed = true
				cur.Done = false
				cur.Size = src.size
			}
		}
		if cur == nil {
			if src.name != "traffic.log" {
				rotated, parseErr := time.ParseInLocation("20060102-150405", strings.TrimSuffix(strings.TrimPrefix(src.name, "traffic.log-"), ".gz"), shanghai)
				if parseErr != nil || rotated.Before(now.Add(-35*24*time.Hour)) {
					warn(&next, now, "archive_horizon", "An untracked archive is outside the 35-day recovery horizon; it was not replayed.", true)
					continue
				}
			}
			if len(next.Files) >= maxFiles {
				warn(&next, now, "cursor_limit", "Traffic cursor limit reached; new files were not accounted.", true)
				continue
			}
			if next.Next == math.MaxUint64 {
				return errors.New("traffic cursor sequence exhausted")
			}
			next.Next++
			key = strconv.FormatUint(next.Next, 10)
			cur = &cursor{Identity: src.identity, Compressed: src.compressed}
			next.Files[key] = cur
		}
		seen[key] = true
		if cur.LastSeen == "" || cur.Name != src.name || cur.Size != src.size || cur.Missing {
			cur.LastSeen = stamp(now)
		}
		cur.Missing = false
		if cur.Offset > 0 && (!prefixMatches(cur, src) || !src.compressed && src.size < cur.Offset || cur.Compressed && cur.Done && cur.Size != src.size) {
			cur.Blocked = true
			warn(&next, now, "source_changed", "A traffic source was truncated or replaced in place; its cursor was quarantined instead of replayed.", true)
		}
		cur.Name = src.name
		cur.Size = src.size
		if cur.Blocked || src.compressed && cur.Done || !src.compressed && cur.Done && src.size == cur.Offset {
			continue
		}
		if c.active != nil && c.active.gz != nil && c.active.key != key {
			// Do not repeatedly discard a long gzip skip merely to revisit an
			// earlier file. Finish the bounded recovery before other unread files.
			catching = true
			continue
		}
		if remaining <= 0 {
			catching = true
			continue
		}
		more, err := c.consume(ctx, &next, key, cur, src, &remaining, now)
		if err != nil {
			c.closeStream()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			cur.Blocked = true
			warn(&next, now, "source_changed", "A traffic source could not be verified at its saved cursor; replay was stopped.", true)
		}
		if more {
			catching = true
		}
	}
	for key, f := range next.Files {
		if !seen[key] && !stillPresent(f, sources) && !f.Missing {
			f.Missing = true
			if !f.Done || f.Name == "traffic.log" {
				warn(&next, now, "source_missing", "A previously tracked traffic file disappeared before its tail could be confirmed; totals may omit requests.", true)
			}
		}
	}
	if c.active != nil && (!seen[c.active.key] || next.Files[c.active.key].Blocked) {
		c.closeStream()
	}
	// Retire vanished identities after 35 days. Such archives are outside the
	// supported recovery horizon and are explicitly rejected below on discovery.
	for key, f := range next.Files {
		last, _ := time.Parse(time.RFC3339Nano, f.LastSeen)
		if f.Missing && now.Sub(last) > 35*24*time.Hour {
			delete(next.Files, key)
		}
	}
	if err := ctx.Err(); err != nil {
		c.closeStream()
		return err
	}
	data, err = json.Marshal(next)
	if err != nil || len(data) > maxStateBytes {
		c.closeStream()
		return errors.New("traffic state exceeded its bounded size")
	}
	if c.persisted && bytes.Equal(previousBytes, data) {
		c.catchingUp = catching
		return nil
	}
	next.UpdatedAt = stamp(now)
	data, err = json.Marshal(next)
	if err != nil || len(data) > maxStateBytes {
		c.closeStream()
		return errors.New("traffic state exceeded its bounded size")
	}
	err = c.write(filepath.Join(c.dir, "traffic-state.json"), append(data, '\n'), atomicfile.Options{})
	if err == nil || atomicfile.IsCommitted(err) {
		c.state = next
		c.catchingUp = catching
		c.persisted = true
	} else {
		c.closeStream()
	}
	return err
}

func (c *Collector) consume(ctx context.Context, s *ledger, key string, cur *cursor, src source, budget *int, now time.Time) (bool, error) {
	if c.active == nil || c.active.key != key {
		c.closeStream()
		f, err := safeOpen(filepath.Join(c.dir, src.name))
		if err != nil {
			return false, err
		}
		id, err := fileIdentity(f)
		if err != nil || id != src.identity {
			f.Close()
			return false, errors.New("source identity changed")
		}
		st := &stream{key: key, f: f}
		var r io.Reader = f
		if src.compressed {
			st.gz, err = gzip.NewReader(f)
			if err != nil {
				f.Close()
				return false, err
			}
			r = st.gz
		} else {
			start := cur.Offset - int64(tailBytes)
			if start < 0 {
				start = 0
			}
			if _, err = f.Seek(start, io.SeekStart); err != nil {
				f.Close()
				return false, err
			}
			st.position = start
		}
		st.r = bufio.NewReaderSize(r, maxLineBytes)
		c.active = st
	}
	st := c.active
	if !st.verified {
		buf := make([]byte, maxLineBytes)
		for st.position < cur.Offset {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if *budget <= 0 {
				return true, nil
			}
			nwant := min(int64(len(buf)), cur.Offset-st.position, int64(*budget))
			n, err := io.ReadFull(st.r, buf[:nwant])
			st.position += int64(n)
			*budget -= n
			st.tail = tail(st.tail, buf[:n])
			if err != nil {
				return false, errors.New("source truncated before checkpoint")
			}
		}
		if cur.Offset > 0 && hash(st.tail) != cur.Tail {
			return false, errors.New("source checkpoint changed")
		}
		st.verified = true
	}
	for *budget > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		part, err := st.r.ReadSlice('\n')
		*budget -= len(part)
		st.position += int64(len(part))
		st.pending = append(st.pending, part...)
		if err == io.EOF {
			cur.Done = len(st.pending) == 0 && !cur.Discard
			if src.compressed && len(st.pending) > 0 {
				warn(s, now, "archive_partial", "A sealed compressed archive ends with an incomplete record; its tail was not counted.", true)
				cur.Blocked = true
			}
			c.closeStream()
			return false, nil
		}
		if err != nil && err != bufio.ErrBufferFull {
			return false, err
		}
		if len(st.pending) > maxLineBytes || cur.Discard || err == bufio.ErrBufferFull {
			cur.Discard = true
			checkpoint(cur, src, st.pending)
			st.tail = tail(st.tail, st.pending)
			cur.Tail = hash(st.tail)
			st.pending = nil
			if err == nil {
				cur.Discard = false
				warn(s, now, "invalid_record", "Invalid or oversized traffic records were skipped; totals are incomplete.", true)
			}
			continue
		}
		if err == nil {
			account(s, st.pending, now)
			checkpoint(cur, src, st.pending)
			st.tail = tail(st.tail, st.pending)
			cur.Tail = hash(st.tail)
			st.pending = nil
			cur.Done = false
		}
	}
	if !src.compressed {
		c.closeStream()
	}
	return true, nil
}
func tail(previous, part []byte) []byte {
	if len(part) >= tailBytes {
		return append(previous[:0], part[len(part)-tailBytes:]...)
	}
	all := append(previous, part...)
	if len(all) > tailBytes {
		all = all[len(all)-tailBytes:]
	}
	return all
}
func checkpoint(cur *cursor, src source, part []byte) {
	cur.Offset += int64(len(part))
	cur.PrefixLen = min(prefixBytes, int(cur.Offset), len(src.prefix))
	cur.Prefix = hash(src.prefix[:cur.PrefixLen])
}

func account(s *ledger, line []byte, now time.Time) {
	var record struct {
		V        int     `json:"v"`
		Service  string  `json:"service"`
		Time     string  `json:"time"`
		Status   int     `json:"status"`
		Received *uint64 `json:"received"`
		Sent     *uint64 `json:"sent"`
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	err := d.Decode(&record)
	var extra any
	at, te := time.Parse(time.RFC3339Nano, record.Time)
	if err != nil || d.Decode(&extra) != io.EOF || record.V != 1 || !serviceID.MatchString(record.Service) || te != nil || at.After(now.Add(24*time.Hour)) || at.In(shanghai).Format("2006-01-02") > now.In(shanghai).Format("2006-01-02") || record.Status < 100 || record.Status > 599 || record.Received == nil || record.Sent == nil {
		warn(s, now, "invalid_record", "Invalid or oversized traffic records were skipped; totals are incomplete.", true)
		return
	}
	v, exists := s.Services[record.Service]
	if !exists && len(s.Services) >= maxServices {
		warn(s, now, "service_limit", "Service accounting limit reached; additional service records were skipped.", true)
		return
	}
	delta := Counters{1, *record.Received, *record.Sent}
	total, ok := add(s.Total, delta)
	service, ok2 := add(v.Total, delta)
	if !ok || !ok2 {
		warn(s, now, "counter_overflow", "Traffic counters reached their numeric limit; further records were not counted.", true)
		return
	}
	s.Total = total
	v.Total = service
	if v.LastSeenAt == "" || later(at, v.LastSeenAt) {
		v.LastSeenAt = stamp(at)
	}
	s.Services[record.Service] = v
	if s.LastRecordAt == "" || later(at, s.LastRecordAt) {
		s.LastRecordAt = stamp(at)
	}
	date := at.In(shanghai).Format("2006-01-02")
	today := now.In(shanghai).Format("2006-01-02")
	oldest := now.In(shanghai).AddDate(0, 0, -29).Format("2006-01-02")
	if date < oldest || date > today {
		return
	}
	day := s.Days[date]
	day.Date = date
	if day.Services == nil {
		day.Services = map[string]Counters{}
	}
	day.Total, _ = add(day.Total, delta)
	day.Services[record.Service], _ = add(day.Services[record.Service], delta)
	s.Days[date] = day
}
func later(t time.Time, s string) bool {
	old, _ := time.Parse(time.RFC3339Nano, s)
	return t.After(old)
}
