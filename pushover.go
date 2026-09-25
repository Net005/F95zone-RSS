package main

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SendPushover posts a notification via Pushover's API. Returns a human-readable
// error string ("" on success) so callers can store it without importing errors.
func SendPushover(cfg Config, title, message, link string) string {
	if !cfg.PushoverEnabled || cfg.PushoverUserKey == "" || cfg.PushoverAPIToken == "" {
		return "Pushover is not configured"
	}
	form := url.Values{
		"token":     {cfg.PushoverAPIToken},
		"user":      {cfg.PushoverUserKey},
		"title":     {title},
		"message":   {message},
		"url":       {link},
		"url_title": {"Open thread"},
		"priority":  {strconv.Itoa(cfg.PushoverPriority)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.pushover.net/1/messages.json", strings.NewReader(form.Encode()))
	if err != nil {
		return err.Error()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "pushover: HTTP " + strconv.Itoa(resp.StatusCode)
	}
	return ""
}
