package store

import (
	"fmt"
	"log/slog"
	"os"
	"sync"

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
	Channels []yamlChannelEntry `yaml:"channels,omitempty"`
	Roles    []yamlRoleEntry    `yaml:"roles,omitempty"`
}

type yamlData struct {
	Guilds []yamlGuildEntry `yaml:"guilds"`
}

// ── YAMLStore ─────────────────────────────────────────────────────────────────

// YAMLStore is a thread-safe, file-backed implementation of Store.
// All bindings are persisted to a YAML file after every mutation.
type YAMLStore struct {
	mu       sync.RWMutex
	path     string
	channels map[channelKey]snowflake.ID
	roles    map[roleKey]snowflake.ID
}

// NewYAMLStore opens (or creates) the YAML file at path and loads existing bindings.
func NewYAMLStore(path string) (*YAMLStore, error) {
	s := &YAMLStore{
		path:     path,
		channels: make(map[channelKey]snowflake.ID),
		roles:    make(map[roleKey]snowflake.ID),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	slog.Info("yaml store loaded", slog.String("path", path),
		slog.Int("channels", len(s.channels)),
		slog.Int("roles", len(s.roles)),
	)
	return s, nil
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
	}
	return nil
}

// save serialises the current state to the YAML file.
// Must be called with mu write-locked.
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

	yd := yamlData{}
	for _, g := range guildSet {
		yd.Guilds = append(yd.Guilds, *g)
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
	if err := s.save(); err != nil {
		slog.Error("yaml store: failed to persist channel binding", slog.Any("err", err))
	}
}

func (s *YAMLStore) UnbindChannel(guildID, userID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.channels, channelKey{userID, guildID})
	if err := s.save(); err != nil {
		slog.Error("yaml store: failed to persist channel unbinding", slog.Any("err", err))
	}
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
	if err := s.save(); err != nil {
		slog.Error("yaml store: failed to persist role binding", slog.Any("err", err))
	}
}

func (s *YAMLStore) UnbindRole(guildID snowflake.ID, roleType RoleType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.roles, roleKey{guildID, roleType})
	if err := s.save(); err != nil {
		slog.Error("yaml store: failed to persist role unbinding", slog.Any("err", err))
	}
}

func (s *YAMLStore) GetBoundRole(guildID snowflake.ID, roleType RoleType) (snowflake.ID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	roleID, ok := s.roles[roleKey{guildID, roleType}]
	return roleID, ok
}
