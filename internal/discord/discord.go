// Package discord posts Halfway's decisions into Discord channels, through
// the channel's own incoming webhook: an organizer pastes its URL, and each
// decision is one message. No bot to host, nothing to read.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"halfway/internal/store"
)

// Platform is how chats connected through this package are marked.
const Platform = "discord"

// ErrNotWebhook is a URL that is not a Discord channel's webhook.
var ErrNotWebhook = errors.New("not a Discord webhook URL")

// ErrRefused is a webhook Discord says no longer exists.
var ErrRefused = errors.New("discord refused the webhook")

type Client struct {
	http *http.Client
	// allow says whether the server may send to a URL. Only Discord's own
	// webhook addresses, over HTTPS: anything else would let a poll's
	// organizer have this server make requests wherever they liked.
	allow func(*url.URL) bool
}

func New() *Client {
	return &Client{http: &http.Client{Timeout: 15 * time.Second}, allow: isDiscord}
}

// NewWith is New with another HTTP client and rule for which URLs may be
// sent to, for tests that stand in for Discord.
func NewWith(h *http.Client, allow func(*url.URL) bool) *Client {
	return &Client{http: h, allow: allow}
}

func isDiscord(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	okHost := host == "discord.com" || host == "discordapp.com" || host == "canary.discord.com" || host == "ptb.discord.com"
	return u.Scheme == "https" && okHost && u.Port() == "" && strings.HasPrefix(u.Path, "/api/webhooks/") && u.User == nil
}

// Clean checks raw is a webhook URL this server may send to, and returns it
// without anything a person may have pasted around it.
func (c *Client) Clean(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !c.allow(u) {
		return "", ErrNotWebhook
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(u.Path, "/api/webhooks/"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ErrNotWebhook
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// Check asks Discord about a webhook, which proves it works, and returns its
// name ("Halfway", or whatever the channel's admins called it).
func (c *Client) Check(ctx context.Context, webhook string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", webhook, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", errors.New("discord: could not reach it") // the URL holds the webhook's secret: keep it out
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
		return "", ErrRefused
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discord: HTTP %d", resp.StatusCode)
	}
	var hook struct {
		Name string `json:"name"`
	}
	json.NewDecoder(resp.Body).Decode(&hook)
	return hook.Name, nil
}

// Send posts text into the webhook's channel. Mentions are switched off, so
// a poll called "@everyone" pings nobody.
func (c *Client) Send(ctx context.Context, webhook, text string) error {
	body, _ := json.Marshal(map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}})
	req, err := http.NewRequestWithContext(ctx, "POST", webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("discord: could not reach it")
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrRefused
	}
	return fmt.Errorf("discord: HTTP %d", resp.StatusCode)
}

// Deliver posts every decision still owed to a Discord channel. One whose
// webhook was deleted is marked broken, so its organizers are asked to
// connect it again; one that is busy is tried again next time.
func (c *Client) Deliver(ctx context.Context, s *store.Store) {
	pending, err := s.Pending()
	if err != nil {
		log.Print(err)
		return
	}
	for _, d := range pending {
		if d.Platform != Platform {
			continue
		}
		u, err := url.Parse(d.Target)
		if err != nil || !c.allow(u) {
			s.ChatBroken(Platform, d.Target)
			continue
		}
		switch err := c.Send(ctx, d.Target, d.Text); {
		case err == nil:
			s.Delivered(d.EventID, d.ChatID)
		case errors.Is(err, ErrRefused):
			s.ChatBroken(Platform, d.Target)
		default:
			log.Print(err)
		}
	}
}
