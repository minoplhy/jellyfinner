package main

import (
	"testing"

	"jellyfinner/internal/config"
)

func TestWebhookUnreachable(t *testing.T) {
	t.Setenv("JELLYFINNER_IN_CONTAINER", "1")
	for _, c := range []struct {
		mode config.Mode
		addr string
		want bool
	}{
		{config.ModeManual, "127.0.0.1:8201", true},
		{config.ModeManual, "0.0.0.0:8201", false},
		{config.ModeAuto, "127.0.0.1:8201", false},
	} {
		cfg := &config.Config{Mode: c.mode, WebhookAddr: c.addr}
		if got := webhookUnreachable(cfg); got != c.want {
			t.Errorf("%s on %s = %v", c.mode, c.addr, got)
		}
	}
	t.Setenv("JELLYFINNER_IN_CONTAINER", "")
	if webhookUnreachable(&config.Config{Mode: config.ModeManual, WebhookAddr: "127.0.0.1:8201"}) {
		t.Error("a host install can reach its own loopback")
	}
}
