package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	notifyConfigFile       = "notify-config.json"
	maxBlockedSettleSec    = 60
	maxDoneSettleSec       = 120
	maxCooldownSec         = 600
	maxMuteDurationMinutes = 1440
	maxPublicURLLen        = 512
	presenceHeartbeatTTL   = 15 * time.Second
)

// notifyCategories toggles individual notification categories.
type notifyCategories struct {
	Blocked     bool `json:"blocked"`
	Done        bool `json:"done"`
	Update      bool `json:"update"`
	CacheCold   bool `json:"cacheCold"`
	MachineLoad bool `json:"machineLoad"`
}

// notifyConfig is notify-config.json: settle windows, per-pane cooldown,
// active-viewer suppression, mute expiry, category toggles, and optional
// deep-link base URL. Gen ties the file to the current auth credential.
type notifyConfig struct {
	BlockedSettleSec int              `json:"blockedSettleSec"`
	DoneSettleSec    int              `json:"doneSettleSec"`
	CooldownSec      int              `json:"cooldownSec"`
	SkipWhileViewing bool             `json:"skipWhileViewing"`
	MutedUntil       string           `json:"mutedUntil,omitempty"`
	Categories       notifyCategories `json:"categories"`
	PublicURL        string           `json:"publicUrl"`
	Gen              string           `json:"gen,omitempty"`
}

type notifyCategoriesUpdate struct {
	Blocked     *bool `json:"blocked"`
	Done        *bool `json:"done"`
	Update      *bool `json:"update"`
	CacheCold   *bool `json:"cacheCold"`
	MachineLoad *bool `json:"machineLoad"`
}

type notifyConfigUpdate struct {
	BlockedSettleSec *int                    `json:"blockedSettleSec"`
	DoneSettleSec    *int                    `json:"doneSettleSec"`
	CooldownSec      *int                    `json:"cooldownSec"`
	SkipWhileViewing *bool                   `json:"skipWhileViewing"`
	MutedUntil       *string                 `json:"mutedUntil"`
	Categories       *notifyCategoriesUpdate `json:"categories"`
	PublicURL        *string                 `json:"publicUrl"`
}

type notifyConfigResponse struct {
	BlockedSettleSec int              `json:"blockedSettleSec"`
	DoneSettleSec    int              `json:"doneSettleSec"`
	CooldownSec      int              `json:"cooldownSec"`
	SkipWhileViewing bool             `json:"skipWhileViewing"`
	MutedUntil       string           `json:"mutedUntil,omitempty"`
	Categories       notifyCategories `json:"categories"`
	PublicURL        string           `json:"publicUrl"`
	Telegram         telegramStatus   `json:"telegram"`
}

func defaultNotifyConfig() notifyConfig {
	return notifyConfig{
		BlockedSettleSec: 5,
		DoneSettleSec:    15,
		CooldownSec:      30,
		SkipWhileViewing: true,
		Categories: notifyCategories{
			Blocked:     true,
			Done:        true,
			Update:      false,
			CacheCold:   false,
			MachineLoad: true,
		},
		PublicURL: "",
	}
}

func (c notifyConfig) SettleDuration(kind string) time.Duration {
	switch kind {
	case "blocked":
		return time.Duration(c.BlockedSettleSec) * time.Second
	case "done":
		return time.Duration(c.DoneSettleSec) * time.Second
	default:
		return 0
	}
}

func (c notifyConfig) CooldownDuration() time.Duration {
	return time.Duration(c.CooldownSec) * time.Second
}

func (c notifyConfig) CategoryEnabled(kind string) bool {
	switch kind {
	case "blocked":
		return c.Categories.Blocked
	case "done":
		return c.Categories.Done
	case "update":
		return c.Categories.Update
	case "cache-cold":
		return c.Categories.CacheCold
	case "machine-load":
		return c.Categories.MachineLoad
	default:
		return false
	}
}

func (c notifyConfig) IsMuted(now time.Time) bool {
	if c.MutedUntil == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, c.MutedUntil)
	if err != nil {
		return false
	}
	return now.Before(until)
}

func validatePublicURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > maxPublicURLLen {
		return errors.New("publicUrl exceeds 512 bytes")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" {
		return errors.New("publicUrl is not a valid URL")
	}
	if u.User != nil {
		return errors.New("publicUrl must not contain userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("publicUrl must not contain query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return errors.New("http publicUrl is only allowed for localhost")
	default:
		return errors.New("publicUrl scheme must be https (or http for localhost)")
	}
}

func validateNotifyConfig(cfg notifyConfig) error {
	if cfg.BlockedSettleSec < 0 || cfg.BlockedSettleSec > maxBlockedSettleSec {
		return &codedError{"invalid_config", "blockedSettleSec must be between 0 and 60", http.StatusBadRequest}
	}
	if cfg.DoneSettleSec < 0 || cfg.DoneSettleSec > maxDoneSettleSec {
		return &codedError{"invalid_config", "doneSettleSec must be between 0 and 120", http.StatusBadRequest}
	}
	if cfg.CooldownSec < 0 || cfg.CooldownSec > maxCooldownSec {
		return &codedError{"invalid_config", "cooldownSec must be between 0 and 600", http.StatusBadRequest}
	}
	if cfg.MutedUntil != "" {
		if _, err := time.Parse(time.RFC3339, cfg.MutedUntil); err != nil {
			return &codedError{"invalid_config", "mutedUntil must be RFC3339 or empty", http.StatusBadRequest}
		}
	}
	if err := validatePublicURL(cfg.PublicURL); err != nil {
		return &codedError{"invalid_config", err.Error(), http.StatusBadRequest}
	}
	return nil
}

func (s *pushStore) loadNotifyConfig() error {
	s.cfg = defaultNotifyConfig()
	path := filepath.Join(s.dir, notifyConfigFile)
	data, err := readStateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", notifyConfigFile, err)
	}
	var cfg notifyConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("push: %s is corrupt, starting with default notify config: %v", notifyConfigFile, err)
		if err := os.Rename(path, path+".bad"); err != nil {
			return fmt.Errorf("move %s aside: %w", notifyConfigFile, err)
		}
		return nil
	}
	if cfg.Gen != s.gen || validateNotifyConfig(cfg) != nil {
		log.Printf("push: dropping %s of another password or invalid config", notifyConfigFile)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", notifyConfigFile, err)
		}
		return nil
	}
	s.cfg = cfg
	return nil
}

func (s *pushStore) saveNotifyConfigLocked() error {
	s.cfg.Gen = s.gen
	data, err := json.Marshal(s.cfg)
	if err != nil {
		return err
	}
	return writeStateFile("push", s.dir, "notify-config", filepath.Join(s.dir, notifyConfigFile), data, false)
}

func (s *pushStore) notifyConfig() notifyConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	if cfg.MutedUntil != "" && !cfg.IsMuted(s.now()) {
		cfg.MutedUntil = ""
	}
	cfg.Gen = ""
	return cfg
}

func (s *pushStore) setNotifyConfig(cfg notifyConfig) error {
	if err := validateNotifyConfig(cfg); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.cfg
	s.cfg = cfg
	if err := s.saveNotifyConfigLocked(); err != nil {
		s.cfg = prev
		return err
	}
	return nil
}

func (s *pushStore) updateNotifyConfig(upd notifyConfigUpdate) (notifyConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg
	if upd.BlockedSettleSec != nil {
		next.BlockedSettleSec = *upd.BlockedSettleSec
	}
	if upd.DoneSettleSec != nil {
		next.DoneSettleSec = *upd.DoneSettleSec
	}
	if upd.CooldownSec != nil {
		next.CooldownSec = *upd.CooldownSec
	}
	if upd.SkipWhileViewing != nil {
		next.SkipWhileViewing = *upd.SkipWhileViewing
	}
	if upd.MutedUntil != nil {
		next.MutedUntil = *upd.MutedUntil
	}
	if upd.PublicURL != nil {
		next.PublicURL = strings.TrimSpace(*upd.PublicURL)
	}
	if upd.Categories != nil {
		if upd.Categories.Blocked != nil {
			next.Categories.Blocked = *upd.Categories.Blocked
		}
		if upd.Categories.Done != nil {
			next.Categories.Done = *upd.Categories.Done
		}
		if upd.Categories.Update != nil {
			next.Categories.Update = *upd.Categories.Update
		}
		if upd.Categories.CacheCold != nil {
			next.Categories.CacheCold = *upd.Categories.CacheCold
		}
		if upd.Categories.MachineLoad != nil {
			next.Categories.MachineLoad = *upd.Categories.MachineLoad
		}
	}
	if err := validateNotifyConfig(next); err != nil {
		return notifyConfig{}, err
	}
	prev := s.cfg
	s.cfg = next
	if err := s.saveNotifyConfigLocked(); err != nil {
		s.cfg = prev
		return notifyConfig{}, err
	}
	out := s.cfg
	out.Gen = ""
	return out, nil
}

func (s *pushStore) setMute(durationMinutes int) (string, error) {
	if durationMinutes < 0 || durationMinutes > maxMuteDurationMinutes {
		return "", &codedError{"invalid_duration", "durationMinutes must be between 0 and 1440", http.StatusBadRequest}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.cfg
	if durationMinutes == 0 {
		s.cfg.MutedUntil = ""
	} else {
		s.cfg.MutedUntil = s.now().UTC().Add(time.Duration(durationMinutes) * time.Minute).Format(time.RFC3339)
	}
	if err := s.saveNotifyConfigLocked(); err != nil {
		s.cfg = prev
		return "", err
	}
	return s.cfg.MutedUntil, nil
}
