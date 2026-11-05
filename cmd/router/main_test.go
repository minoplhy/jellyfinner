package main

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func good() map[string]string {
	return map[string]string{
		"ORCHESTRATOR_URL":     "https://control.example.com",
		"NODE_TOKEN":           "0123456789abcdef0123456789abcdef",
		"JELLYFIN_URL":         "http://jellyfin:8096/jf/",
		"JELLYFIN_TOKEN":       "key",
		"JELLYFIN_LIBRARY_DIR": "/media/",
	}
}

func TestLoadSettings(t *testing.T) {
	s, err := load(env(good()))
	if err != nil {
		t.Fatal(err)
	}
	if s.jellyfinURL != "http://jellyfin:8096/jf" || s.libraryDir != "/media" || s.addr != "127.0.0.1:8204" {
		t.Errorf("settings = %+v", s)
	}

	// Every missing setting is named at once, so one fix is enough.
	if _, err := load(env(map[string]string{})); err == nil ||
		err.Error() != "JELLYFIN_LIBRARY_DIR, JELLYFIN_TOKEN, JELLYFIN_URL, NODE_TOKEN, ORCHESTRATOR_URL not set" {
		t.Errorf("empty: %v", err)
	}
	for key, value := range map[string]string{"JELLYFIN_LIBRARY_DIR": "media", "JELLYFIN_URL": "jellyfin:8096"} {
		m := good()
		m[key] = value
		if _, err := load(env(m)); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q: %v", key, value, err)
		}
	}
}
