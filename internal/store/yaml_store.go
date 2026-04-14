package store

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"gopkg.in/yaml.v3"
)

// ── YAML data model ───────────────────────────────────────────────────────────

type yamlChannelEntry struct {
	UserID    uint64 `yaml:"user_id"`
	ChannelID uint64 `yaml:"channel_id"`
}

type yamlRoleEntry struct {
	RoleType RoleType `yaml:"role_type"`
	RoleID   uint64   `yaml:"role_id"`
}

type yamlGuildEntry struct {
	GuildID  uint64             `yaml:"guild_id"`
	AllyCode string             `yaml:"ally_code,omitempty"`
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
	channels   map[channelKey]snowflake.ID
	roles      map[roleKey]snowflake.ID
	relayCodes map[snowflake.ID]string

	dirtyCh chan struct{} // signals the flush goroutine
	done    chan struct{} // closed by Close to stop the flush goroutine
}

// NewYAMLStore opens (or creates) the YAML file at path and loads existing bindings.
func NewYAMLStore(path string) (*YAMLStore, error) {
	s := &YAMLStore{
		path:       path,
		channels:   make(map[channelKey]snowflake.ID),
		roles:      make(map[roleKey]snowflake.ID),
		relayCodes: make(map[snowflake.ID]string),
		dirtyCh:    make(chan struct{}, 1),
		done:       make(chan struct{}),
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
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case <-s.done:
			// Final flush on shutdown.
			s.mu.RLock()
			err := s.save()
			s.mu.RUnlock()
			if err != nil {
				slog.Error("yaml store: final flush failed", slog.Any("err", err))
			}
			return
		case <-s.dirtyCh:
			timer.Reset(saveDebounce)
		case <-timer.C:
			s.mu.RLock()
			if err := s.save(); err != nil {
				slog.Error("yaml store: debounced flush failed", slog.Any("err", err))
			}
			s.mu.RUnlock()
		}
	}
}

// markDirty signals the flush goroutine that the in-memory state has changed.
// Must be called with mu write-locked (the caller already holds it).
func (s *YAMLStore) markDirty() {
	select {
	case s.dirtyCh <- struct{}{}:
	default:
	}
}

// Close flushes any pending writes and stops the background goroutine.
func (s *YAMLStore) Close() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
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
			s.channels[channelKey{snowflake.ID(c.UserID), guildID}] = snowflake.ID(c.ChannelID)
		}
		for _, r := range g.Roles {
			s.roles[roleKey{guildID, r.RoleType}] = snowflake.ID(r.RoleID)
		}
		if g.AllyCode != "" {
			s.relayCodes[guildID] = g.AllyCode
		}
	}
	return nil
}

// save serialises the current state to the YAML file.
// Must be called with mu at least read-locked.
func (s *YAMLStore) save() error {
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
		g := ensureGuild(k.guildID)
		g.Channels = append(g.Channels, yamlChannelEntry{
			UserID:    uint64(k.userID),
			ChannelID: uint64(v),
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
		return fmt.Errorf("yaml store: marshal failed: %w", err)
	}
	if err := os.WriteFile(s.path, out, 0o644); err != nil {
		return fmt.Errorf("yaml store: write failed: %w", err)
	}
	return nil
}

// ── Store interface ───────────────────────────────────────────────────────────

func (s *YAMLStore) BindChannel(guildID, userID, channelID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[channelKey{userID, guildID}] = channelID
	s.markDirty()
}

func (s *YAMLStore) UnbindChannel(guildID, userID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.channels, channelKey{userID, guildID})
	s.markDirty()
}

func (s *YAMLStore) GetBoundChannel(guildID, userID snowflake.ID) (snowflake.ID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch, ok := s.channels[channelKey{userID, guildID}]
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
