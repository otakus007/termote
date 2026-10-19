package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	telegramFile           = "telegram.json"
	defaultTelegramAPIBase = "https://api.telegram.org"
	telegramTimeout        = 10 * time.Second
	maxNotifyNameRunes     = 64
)

var (
	telegramBotTokenRe = regexp.MustCompile(`^[A-Za-z0-9:_-]{3,256}$`)
	telegramChatIDRe   = regexp.MustCompile(`^[A-Za-z0-9_@-]{1,128}$`)
	telegramURLBotRe   = regexp.MustCompile(`/bot[^/\s"]+`)
)

// telegramConfig is telegram.json: Telegram Bot token, target chat ID, and
// whether Telegram notifications are enabled. Bound to the auth credential via Gen.
type telegramConfig struct {
	Enabled  bool   `json:"enabled"`
	BotToken string `json:"botToken"`
	ChatID   string `json:"chatId"`
	Gen      string `json:"gen,omitempty"`
}

// telegramStatus is the redacted view returned by GET /api/mux/notify/config.
// Neither BotToken nor the full ChatID is ever exposed.
type telegramStatus struct {
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
	ChatIDTail string `json:"chatIdTail"`
}

type telegramUpdateRequest struct {
	BotToken string `json:"botToken"`
	ChatID   string `json:"chatId"`
	Enabled  *bool  `json:"enabled"`
}

var (
	telegramClientOnce sync.Once
	defaultTGClient    *http.Client
)

func telegramHTTPClient() *http.Client {
	telegramClientOnce.Do(func() {
		defaultTGClient = &http.Client{
			Timeout: telegramTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: 10 * time.Second,
					Control: pushDialControl,
				}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		}
	})
	return defaultTGClient
}

func chatIDTail(chatID string) string {
	r := []rune(strings.TrimSpace(chatID))
	if len(r) == 0 {
		return ""
	}
	if len(r) > 4 {
		return string(r[len(r)-4:])
	}
	if len(r) == 1 {
		return "*"
	}
	return string(r[len(r)/2:])
}

func validateTelegramCredentials(botToken, chatID string) error {
	if !telegramBotTokenRe.MatchString(botToken) {
		return &codedError{"invalid_telegram", "botToken is invalid", http.StatusBadRequest}
	}
	if !telegramChatIDRe.MatchString(chatID) {
		return &codedError{"invalid_telegram", "chatId is invalid", http.StatusBadRequest}
	}
	return nil
}

func redactTelegramSecret(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "[REDACTED]")
	}
	return telegramURLBotRe.ReplaceAllString(s, "/bot[REDACTED]")
}

// cleanNotifyText strips control characters, bidi overrides/isolates, and
// zero-width characters from a group, tab, or agent label and caps its length.
func cleanNotifyText(s string) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		switch {
		case r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\u200e' || r == '\u200f':
			continue
		case r >= '\u202a' && r <= '\u202e':
			continue
		case r == '\u2060' || (r >= '\u2066' && r <= '\u2069') || r == '\ufeff':
			continue
		}
		b.WriteRune(r)
		count++
		if count >= maxNotifyNameRunes {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func (s *pushStore) loadTelegram() error {
	s.telegram = telegramConfig{}
	path := filepath.Join(s.dir, telegramFile)
	data, err := readStateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", telegramFile, err)
	}
	var cfg telegramConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("push: %s is corrupt, starting without telegram config", telegramFile)
		if err := os.Rename(path, path+".bad"); err != nil {
			return fmt.Errorf("move %s aside: %w", telegramFile, err)
		}
		return nil
	}
	if cfg.Gen != s.gen || validateTelegramCredentials(cfg.BotToken, cfg.ChatID) != nil {
		log.Printf("push: dropping %s of another password or invalid credentials", telegramFile)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", telegramFile, err)
		}
		return nil
	}
	s.telegram = cfg
	return nil
}

func (s *pushStore) saveTelegramLocked() error {
	s.telegram.Gen = s.gen
	data, err := json.Marshal(s.telegram)
	if err != nil {
		return err
	}
	return writeStateFile("push", s.dir, "telegram", filepath.Join(s.dir, telegramFile), data, true)
}

func (s *pushStore) telegramStatus() telegramStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.telegramStatusLocked()
}

func (s *pushStore) telegramStatusLocked() telegramStatus {
	configured := s.telegram.BotToken != "" && s.telegram.ChatID != ""
	if !configured {
		return telegramStatus{}
	}
	return telegramStatus{
		Configured: true,
		Enabled:    s.telegram.Enabled,
		ChatIDTail: chatIDTail(s.telegram.ChatID),
	}
}

func (s *pushStore) telegramActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.telegram.Enabled && s.telegram.BotToken != "" && s.telegram.ChatID != ""
}

func (s *pushStore) updateTelegram(req telegramUpdateRequest) (telegramStatus, error) {
	botToken := strings.TrimSpace(req.BotToken)
	chatID := strings.TrimSpace(req.ChatID)

	s.mu.Lock()
	defer s.mu.Unlock()

	if botToken == "" && chatID == "" && s.telegram.BotToken != "" && s.telegram.ChatID != "" {
		botToken = s.telegram.BotToken
		chatID = s.telegram.ChatID
	}
	if err := validateTelegramCredentials(botToken, chatID); err != nil {
		return telegramStatus{}, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	prev := s.telegram
	s.telegram = telegramConfig{
		Enabled:  enabled,
		BotToken: botToken,
		ChatID:   chatID,
		Gen:      s.gen,
	}
	if err := s.saveTelegramLocked(); err != nil {
		s.telegram = prev
		return telegramStatus{}, err
	}
	return s.telegramStatusLocked(), nil
}

func (s *pushStore) removeTelegram() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.telegram = telegramConfig{}
	s.telegramBackoff = time.Time{}
	path := filepath.Join(s.dir, telegramFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type telegramMessagePayload struct {
	ChatID      string               `json:"chat_id"`
	Text        string               `json:"text"`
	ReplyMarkup *telegramReplyMarkup `json:"reply_markup,omitempty"`
}

type telegramReplyMarkup struct {
	InlineKeyboard [][]telegramInlineButton `json:"inline_keyboard"`
}

type telegramInlineButton struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type paneNotifyMeta struct {
	groupName string
	tabName   string
	agent     string
}

func isLoopbackPublicURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func formatTelegramEvent(e pushEvent, meta paneNotifyMeta, publicURL string) (string, *telegramReplyMarkup) {
	group := cleanNotifyText(meta.groupName)
	if group == "" {
		group = cleanNotifyText(e.GroupID)
	}
	tab := cleanNotifyText(meta.tabName)
	if tab == "" {
		tab = cleanNotifyText(e.TabID)
	}
	agent := cleanNotifyText(meta.agent)
	if agent == "" {
		agent = "Agent"
	}

	var scope string
	switch {
	case group != "" && tab != "":
		scope = fmt.Sprintf("[%s / %s] ", group, tab)
	case group != "":
		scope = fmt.Sprintf("[%s] ", group)
	case tab != "":
		scope = fmt.Sprintf("[%s] ", tab)
	}

	var headline string
	switch e.Kind {
	case "blocked":
		headline = fmt.Sprintf("⚠️ %s%s needs you", scope, agent)
	case "done":
		headline = fmt.Sprintf("✅ %s%s finished its turn", scope, agent)
	case "update":
		headline = "⬆️ Termote update available"
	case "cache-cold":
		headline = fmt.Sprintf("❄️ %s%s prompt cache is cooling", scope, agent)
	case "machine-load":
		headline = "🔥 High machine load on Termote host"
	default:
		headline = fmt.Sprintf("🔔 %s%s notification", scope, agent)
	}

	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if base == "" || isLoopbackPublicURL(base) {
		return headline, nil
	}
	deepLink := base + "/"
	if e.PaneID != "" {
		deepLink = base + "/#pane=" + url.QueryEscape(e.PaneID)
	}
	text := headline + "\n" + deepLink
	var markup *telegramReplyMarkup
	if strings.HasPrefix(deepLink, "https://") {
		markup = &telegramReplyMarkup{
			InlineKeyboard: [][]telegramInlineButton{
				{{Text: "Open in Termote", URL: deepLink}},
			},
		}
	}
	return text, markup
}

func (s *pushStore) sendTelegramEvent(ctx context.Context, e pushEvent, meta paneNotifyMeta) error {
	s.mu.Lock()
	cfg := s.telegram
	pubURL := s.cfg.PublicURL
	backoffUntil := s.telegramBackoff
	now := s.now()
	s.mu.Unlock()

	if !cfg.Enabled || cfg.BotToken == "" || cfg.ChatID == "" {
		return nil
	}
	if now.Before(backoffUntil) {
		return nil
	}
	text, markup := formatTelegramEvent(e, meta, pubURL)
	if err := s.postTelegramMessage(ctx, cfg.BotToken, cfg.ChatID, text, markup); err != nil {
		return errors.New(redactTelegramSecret(err.Error(), cfg.BotToken))
	}
	return nil
}

func (s *pushStore) sendTelegramTest(ctx context.Context) error {
	return s.sendTelegramTestWith(ctx, "", "")
}

func (s *pushStore) sendTelegramTestWith(ctx context.Context, botToken, chatID string) error {
	s.mu.Lock()
	cfg := s.telegram
	pubURL := s.cfg.PublicURL
	s.mu.Unlock()

	botToken = strings.TrimSpace(botToken)
	chatID = strings.TrimSpace(chatID)
	if botToken == "" {
		botToken = cfg.BotToken
	}
	if chatID == "" {
		chatID = cfg.ChatID
	}
	if botToken == "" || chatID == "" {
		return &codedError{"telegram_not_configured", "Telegram bot token and chat ID are not configured", http.StatusBadRequest}
	}
	if err := validateTelegramCredentials(botToken, chatID); err != nil {
		return err
	}
	text := "🔔 Termote test notification"
	var markup *telegramReplyMarkup
	if base := strings.TrimRight(strings.TrimSpace(pubURL), "/"); base != "" && !isLoopbackPublicURL(base) {
		deepLink := base + "/"
		text += "\n" + deepLink
		if strings.HasPrefix(deepLink, "https://") {
			markup = &telegramReplyMarkup{
				InlineKeyboard: [][]telegramInlineButton{
					{{Text: "Open in Termote", URL: deepLink}},
				},
			}
		}
	}
	if err := s.postTelegramMessage(ctx, botToken, chatID, text, markup); err != nil {
		safeMsg := redactTelegramSecret(err.Error(), botToken)
		return &codedError{"telegram_send_failed", safeMsg, http.StatusBadGateway}
	}
	return nil
}

func (s *pushStore) postTelegramMessage(ctx context.Context, botToken, chatID, text string, markup *telegramReplyMarkup) error {
	base := s.telegramBaseURL
	if base == "" {
		base = defaultTelegramAPIBase
	}
	endpoint := strings.TrimRight(base, "/") + "/bot" + botToken + "/sendMessage"

	body, err := json.Marshal(telegramMessagePayload{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: markup,
	})
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, telegramTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New(redactTelegramSecret(err.Error(), botToken))
	}
	req.Header.Set("Content-Type", "application/json")

	client := s.telegramHTTP
	if client == nil {
		client = telegramHTTPClient()
	}

	resp, err := client.Do(req)
	if err != nil {
		return errors.New(redactTelegramSecret(err.Error(), botToken))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		now := s.now()
		d := parseRetryAfter(resp.Header.Get("Retry-After"), now)
		if d <= 0 {
			d = pushDefaultBackoff
		}
		s.mu.Lock()
		s.telegramBackoff = now.Add(d)
		s.mu.Unlock()
		return fmt.Errorf("telegram rate limited (HTTP 429)")
	}
	return fmt.Errorf("telegram API returned HTTP %d", resp.StatusCode)
}
