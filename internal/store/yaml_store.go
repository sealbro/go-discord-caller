package store

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"gopkg.in/yaml.v3"
)

// ── YAML data model ───────────────────────────────────────────────────────────

type yamlChannelEntry struct {
	UserID    uint64 `yaml:"user_id"`
	ChannelID uint64 `yaml:"channel_id,omitempty"`
	Disabled  bool   `yaml:"disabled,omitempty"`
}

type yamlRoleEntry struct {
	RoleType RoleType `yaml:"role_type"`
	RoleID   uint64   `yaml:"role_id"`
}

type yamlGuildEntry struct {
	GuildID  uint64             `yaml:"guild_id"`
	AllyCode string             `yaml:"ally_code,omitempty"`
	Locale   string             `yaml:"locale,omitempty"`
	Channels []yamlChannelEntry `yaml:"channels,omitempty"`
	Roles    []yamlRoleEntry    `yaml:"roles,omitempty"`
}

type yamlData struct {
	Guilds []yamlGuildEntry `yaml:"guilds"`
}

// ── YAMLStore ─────────────────────────────────────────────────────────────────

const saveDebounce = 500 * time.Millisecond

// YAMLStore is a thread-safe, file-backed implementation of Store.
// Writes are debounced: mutations mark the store dirty and a background
// goroutine flushes to disk after saveDebounce of inactivity.
type YAMLStore struct {
	mu         sync.RWMutex
	path       string
	channels   map[botKey]snowflake.ID
	roles      map[roleKey]snowflake.ID
	relayCodes map[snowflake.ID]string
	locales    map[snowflake.ID]string
	disabled   map[botKey]struct{}
	closed     bool
	closeOnce  sync.Once
	// snapshots numbers every serialised state, in the order the states were
	// taken under mu. Written with mu held, at least for reading.
	snapshots atomic.Uint64

	// saveMu orders the file writes, which happen outside mu so that a slow
	// disk never stalls a lookup. Never acquire mu while holding it.
	saveMu    sync.Mutex
	saved     uint64 // newest snapshot on disk; guarded by saveMu
	writeFile func(path string, data []byte) error

	dirtyCh chan struct{} // signals the flush goroutine
	done    chan struct{} // closed by Close to stop the flush goroutine
	flushed chan struct{} // closed by flushLoop after the final save completes
}

// NewYAMLStore opens (or creates) the YAML file at path and loads existing bindings.
func NewYAMLStore(path string) (*YAMLStore, error) {
	s := &YAMLStore{
		path:       path,
		channels:   make(map[botKey]snowflake.ID),
		roles:      make(map[roleKey]snowflake.ID),
		relayCodes: make(map[snowflake.ID]string),
		locales:    make(map[snowflake.ID]string),
		disabled:   make(map[botKey]struct{}),
		dirtyCh:    make(chan struct{}, 1),
		done:       make(chan struct{}),
		flushed:    make(chan struct{}),
		writeFile:  writeFileAtomic,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	slog.Info("yaml store loaded", slog.String("path", path),
		slog.Int("channels", len(s.channels)),
		slog.Int("roles", len(s.roles)),
	)
	go s.flushLoop()
	return s, nil
}

// flushLoop runs in a background goroutine and coalesces rapid mutations
// into a single file write after saveDebounce of quiet time.
func (s *YAMLStore) flushLoop() {
	defer close(s.flushed)
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case <-s.done:
			if err := s.flush(); err != nil {
				slog.Error("yaml store: final flush failed", slog.Any("err", err))
			}
			return
		case <-s.dirtyCh:
			timer.Reset(saveDebounce)
		case <-timer.C:
			if err := s.flush(); err != nil {
				slog.Error("yaml store: debounced flush failed", slog.Any("err", err))
			}
		}
	}
}

// markDirty signals the flush goroutine that the in-memory state has changed.
// After Close the flush goroutine is gone, so the change is written
// synchronously instead of being dropped.
// Must be called with mu write-locked (the caller already holds it).
func (s *YAMLStore) markDirty() {
	if s.closed {
		snap, err := s.snapshot()
		if err == nil {
			err = s.persist(snap)
		}
		if err != nil {
			slog.Error("yaml store: write after close failed", slog.Any("err", err))
		}
		return
	}
	select {
	case s.dirtyCh <- struct{}{}:
	default:
	}
}

// Close flushes any pending writes and blocks until the final save completes.
// Writes made after Close are saved synchronously. Close is idempotent.
func (s *YAMLStore) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.closeOnce.Do(func() { close(s.done) })
	<-s.flushed
}

// load reads the YAML file and populates the in-memory maps.
// A missing file is treated as a fresh, empty store.
func (s *YAMLStore) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil // first run — file will be created on first write
	}
	if err != nil {
		return err
	}

	var yd yamlData
	if err := yaml.Unmarshal(data, &yd); err != nil {
		return err
	}
	for _, g := range yd.Guilds {
		guildID := snowflake.ID(g.GuildID)
		for _, c := range g.Channels {
			if c.ChannelID != 0 {
				s.channels[botKey{snowflake.ID(c.UserID), guildID}] = snowflake.ID(c.ChannelID)
			}
			if c.Disabled {
				s.disabled[botKey{snowflake.ID(c.UserID), guildID}] = struct{}{}
			}
		}
		for _, r := range g.Roles {
			s.roles[roleKey{guildID, r.RoleType}] = snowflake.ID(r.RoleID)
		}
		if g.AllyCode != "" {
			if s.relayCodeInUse(g.AllyCode) {
				slog.Warn("yaml store: relay code already used by another guild; generating a new one")
				s.relayCodes[guildID] = uniqueAllyCode(s.relayCodes)
				s.markDirty()
			} else {
				s.relayCodes[guildID] = g.AllyCode
			}
		}
		if g.Locale != "" {
			s.locales[guildID] = g.Locale
		}
	}
	return nil
}

func (s *YAMLStore) relayCodeInUse(code string) bool {
	for _, c := range s.relayCodes {
		if c == code {
			return true
		}
	}
	return false
}

// storeSnapshot is the store's state serialised at one moment.
type storeSnapshot struct {
	seq  uint64
	yaml []byte
}

// flush writes the current state to the YAML file. Only the serialising holds
// mu: an fsync can take long enough to matter, and a writer queued behind a
// read lock blocks every later reader with it.
func (s *YAMLStore) flush() error {
	s.mu.RLock()
	snap, err := s.snapshot()
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return s.persist(snap)
}

// persist writes snap to the YAML file unless a newer snapshot already got
// there, which a write after Close can do while the final flush is in flight.
func (s *YAMLStore) persist(snap storeSnapshot) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if snap.seq < s.saved {
		return nil
	}
	if err := s.writeFile(s.path, snap.yaml); err != nil {
		return err
	}
	s.saved = snap.seq
	return nil
}

// snapshot serialises the current state.
// Must be called with mu at least read-locked.
func (s *YAMLStore) snapshot() (storeSnapshot, error) {
	// Collect all guild IDs present in either map.
	guildSet := make(map[snowflake.ID]*yamlGuildEntry)
	ensureGuild := func(id snowflake.ID) *yamlGuildEntry {
		if g, ok := guildSet[id]; ok {
			return g
		}
		g := &yamlGuildEntry{GuildID: uint64(id)}
		guildSet[id] = g
		return g
	}

	for k, v := range s.channels {
		_, disabled := s.disabled[k]
		g := ensureGuild(k.guildID)
		g.Channels = append(g.Channels, yamlChannelEntry{
			UserID:    uint64(k.userID),
			ChannelID: uint64(v),
			Disabled:  disabled,
		})
	}
	for k, v := range s.roles {
		g := ensureGuild(k.guildID)
		g.Roles = append(g.Roles, yamlRoleEntry{
			RoleType: k.roleType,
			RoleID:   uint64(v),
		})
	}
	for guildID, code := range s.relayCodes {
		g := ensureGuild(guildID)
		g.AllyCode = code
	}
	for guildID, locale := range s.locales {
		g := ensureGuild(guildID)
		g.Locale = locale
	}
	for k := range s.disabled {
		if _, bound := s.channels[k]; bound {
			continue
		}
		g := ensureGuild(k.guildID)
		g.Channels = append(g.Channels, yamlChannelEntry{UserID: uint64(k.userID), Disabled: true})
	}

	// Sort by guild ID for deterministic output.
	guildIDs := make([]snowflake.ID, 0, len(guildSet))
	for id := range guildSet {
		guildIDs = append(guildIDs, id)
	}
	slices.SortFunc(guildIDs, func(a, b snowflake.ID) int {
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	})
	yd := yamlData{}
	for _, id := range guildIDs {
		yd.Guilds = append(yd.Guilds, *guildSet[id])
	}

	out, err := yaml.Marshal(&yd)
	if err != nil {
		return storeSnapshot{}, fmt.Errorf("yaml store: marshal failed: %w", err)
	}
	return storeSnapshot{seq: s.snapshots.Add(1), yaml: out}, nil
}

// writeFileAtomic replaces the file at path with out: it writes a temp file in
// the same directory, then renames it. os.Rename is atomic on POSIX and
// prevents partial/empty files if the process is killed between truncate and
// write-complete.
func writeFileAtomic(path string, out []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".store-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("yaml store: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("yaml store: write temp file: %w", err)
	}
	// Without a sync, a crash shortly after the rename can leave an empty or
	// truncated store.yaml: the rename may reach disk before the data does.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("yaml store: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("yaml store: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("yaml store: rename failed: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("yaml store: sync dir: %w", err)
	}
	return nil
}

// syncDir makes a rename inside dir durable.
func syncDir(dir string) error {
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// ── Store interface ───────────────────────────────────────────────────────────

func (s *YAMLStore) BindChannel(guildID, userID, channelID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[botKey{userID, guildID}] = channelID
	s.markDirty()
}

func (s *YAMLStore) UnbindChannel(guildID, userID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.channels, botKey{userID, guildID})
	s.markDirty()
}

func (s *YAMLStore) GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch, ok := s.channels[botKey{userID, guildID}]
	return ch, ok
}

func (s *YAMLStore) BindRole(guildID snowflake.ID, roleType RoleType, roleID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roles[roleKey{guildID, roleType}] = roleID
	s.markDirty()
}

func (s *YAMLStore) UnbindRole(guildID snowflake.ID, roleType RoleType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.roles, roleKey{guildID, roleType})
	s.markDirty()
}

func (s *YAMLStore) GetBoundRole(guildID snowflake.ID, roleType RoleType) (snowflake.ID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	roleID, ok := s.roles[roleKey{guildID, roleType}]
	return roleID, ok
}

func (s *YAMLStore) GetOrCreateAllyCode(guildID snowflake.ID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code, ok := s.relayCodes[guildID]; ok {
		return code
	}
	code := uniqueAllyCode(s.relayCodes)
	s.relayCodes[guildID] = code
	s.markDirty()
	return code
}

func (s *YAMLStore) GetAllyCode(guildID snowflake.ID) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	code, ok := s.relayCodes[guildID]
	return code, ok
}

func (s *YAMLStore) ResetAllyCode(guildID snowflake.ID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.relayCodes, guildID)
	code := uniqueAllyCode(s.relayCodes)
	s.relayCodes[guildID] = code
	s.markDirty()
	return code
}

func (s *YAMLStore) BindLocale(guildID snowflake.ID, locale string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if locale == "" {
		delete(s.locales, guildID)
	} else {
		s.locales[guildID] = locale
	}
	s.markDirty()
}

func (s *YAMLStore) UnbindLocale(guildID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.locales, guildID)
	s.markDirty()
}

func (s *YAMLStore) GetLocale(guildID snowflake.ID) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	loc, ok := s.locales[guildID]
	return loc, ok
}

func (s *YAMLStore) SetSpeakerEnabled(guildID, userID snowflake.ID, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if enabled {
		delete(s.disabled, botKey{userID, guildID})
	} else {
		s.disabled[botKey{userID, guildID}] = struct{}{}
	}
	s.markDirty()
}

func (s *YAMLStore) IsSpeakerEnabled(guildID, userID snowflake.ID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, disabled := s.disabled[botKey{userID, guildID}]
	return !disabled
}
