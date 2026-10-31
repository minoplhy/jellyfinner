package edge

import (
	"crypto/md5"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

var hash = strings.Repeat("a", 64)

func TestValidHash(t *testing.T) {
	for s, want := range map[string]bool{
		hash: true, strings.Repeat("A", 64): false, "abc": false,
		"../../etc/passwd": false, "": false,
		strings.Repeat("a", 63) + "/": false, hash + "\n": false,
	} {
		if ValidHash(s) != want {
			t.Errorf("ValidHash(%q) != %v", s, want)
		}
	}
}

func TestSignURLMatchesNginxFormula(t *testing.T) {
	expires := time.Unix(2147483647, 0)
	got := SignURL("https://edge1.example.com/", "secret", hash, expires)
	sum := md5.Sum([]byte("2147483647/e/" + hash + ".mp4 secret"))
	want := "https://edge1.example.com/e/" + hash + ".mp4?md5=" + base64.RawURLEncoding.EncodeToString(sum[:]) + "&expires=2147483647"
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	sig, _, _ := strings.Cut(strings.Split(got, "md5=")[1], "&")
	if strings.ContainsAny(sig, "+/=") {
		t.Error("nginx needs URL-safe base64 without padding")
	}
}

func TestDownloadToken(t *testing.T) {
	now := time.Unix(1000, 0)
	token := DownloadToken("key", "edge1", hash, now.Add(time.Hour))
	other := strings.Repeat("b", 64)
	if err := CheckDownloadToken("key", "edge1", hash, token, now); err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]error{
		"other node": CheckDownloadToken("key", "edge2", hash, token, now),
		"other file": CheckDownloadToken("key", "edge1", other, token, now),
		"other key":  CheckDownloadToken("nope", "edge1", hash, token, now),
		"expired":    CheckDownloadToken("key", "edge1", hash, token, now.Add(2*time.Hour)),
		"malformed":  CheckDownloadToken("key", "edge1", hash, "junk", now),
		"bad expiry": CheckDownloadToken("key", "edge1", hash, "x.y", now),
		"extended":   CheckDownloadToken("key", "edge1", hash, "99999999999."+strings.Split(token, ".")[1], now),
	} {
		if check == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
