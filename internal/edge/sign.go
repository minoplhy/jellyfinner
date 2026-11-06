// Package edge holds what edge delivery shares between the orchestrator and
// the router: signed viewer URLs, download tokens, and hash validation.
package edge

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidHash reports whether s is a lowercase SHA-256, the only name a file on
// a node ever has.
func ValidHash(s string) bool { return hashPattern.MatchString(s) }

// FilePath is the URL path of a file on a node.
func FilePath(hash string) string { return "/e/" + hash + ".mp4" }

// SignURL builds a viewer URL that `edge-agent serve` checks, or nginx's
// secure_link module if configured with
//
//	secure_link $arg_md5,$arg_expires;
//	secure_link_md5 "$secure_link_expires$uri <key>";
//
// MD5 is what that module offers. The key is per node, so a leak only allows
// URLs on that node.
func SignURL(baseURL, key, hash string, expires time.Time) string {
	path := FilePath(hash)
	unix := strconv.FormatInt(expires.Unix(), 10)
	return fmt.Sprintf("%s%s?md5=%s&expires=%s", strings.TrimSuffix(baseURL, "/"), path, signature(key, path, unix), unix)
}

// HealthPath is a node's health check. Signed like a file URL, over this
// canonical path, it also answers with the node's build in BuildHeader, so
// only the orchestrator learns the version and anyone else just gets "ok".
const (
	HealthPath  = "/health"
	BuildHeader = "X-Jellyfinner-Build"
)

// SignHealthURL is SignURL for the health check.
func SignHealthURL(baseURL, key string, expires time.Time) string {
	unix := strconv.FormatInt(expires.Unix(), 10)
	return fmt.Sprintf("%s%s?md5=%s&expires=%s", strings.TrimSuffix(baseURL, "/"), HealthPath, signature(key, HealthPath, unix), unix)
}

func signature(key, path, unix string) string {
	sum := md5.Sum([]byte(unix + path + " " + key))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Errors from CheckSignedURL.
var (
	ErrBadSignature = errors.New("bad signature")
	ErrExpired      = errors.New("signed URL expired")
)

// CheckSignedURL validates what SignURL produced, with the same rules as
// secure_link, so a node can use either. The signature is checked first, so
// an expiry that was tampered with reads as a bad signature.
func CheckSignedURL(key, path, sig, expires string, now time.Time) error {
	unix, err := strconv.ParseInt(expires, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	want := signature(key, path, expires)
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return ErrBadSignature
	}
	if now.Unix() > unix {
		return ErrExpired
	}
	return nil
}

// DownloadToken authorises one node to fetch one file until it expires.
func DownloadToken(key, node, hash string, expires time.Time) string {
	unix := strconv.FormatInt(expires.Unix(), 10)
	return unix + "." + tokenMAC(key, node, hash, unix)
}

// CheckDownloadToken validates a token for this exact node and file.
func CheckDownloadToken(key, node, hash, token string, now time.Time) error {
	unix, mac, ok := strings.Cut(token, ".")
	if !ok {
		return errors.New("malformed token")
	}
	expires, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return errors.New("malformed token")
	}
	if !hmac.Equal([]byte(mac), []byte(tokenMAC(key, node, hash, unix))) {
		return errors.New("invalid token")
	}
	if now.Unix() > expires {
		return errors.New("expired token")
	}
	return nil
}

func tokenMAC(key, node, hash, expires string) string {
	h := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(h, "%s\x00%s\x00%s", node, hash, expires)
	return hex.EncodeToString(h.Sum(nil))
}
