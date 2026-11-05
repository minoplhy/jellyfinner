package cloud

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"text/template"
)

//go:embed templates/boot.sh
var bootScript string

var boot = template.Must(template.New("boot").Parse(bootScript))

// imagePattern is a registry reference: lowercase name, optional tag or digest.
var imagePattern = regexp.MustCompile(`^[a-z0-9]+([._/:@-][a-zA-Z0-9]+)*$`)

// tokenPattern matches the node tokens the orchestrator issues.
var tokenPattern = regexp.MustCompile(`^[0-9a-f]{32,128}$`)

// BootValues fill the boot script.
type BootValues struct {
	Image string // worker image reference
	URL   string // how the server reaches the control API, https only
	Token string // this server's own node token
}

// UserData renders the script that installs Docker if needed and starts the
// worker. Every value is checked and then quoted for YAML, so nothing can
// break out of its line or of the here-document.
func UserData(v BootValues) (string, error) {
	if !imagePattern.MatchString(v.Image) {
		return "", fmt.Errorf("worker image %q is not a valid image reference", v.Image)
	}
	u, err := url.Parse(v.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("control URL %q must be an https address that rented servers can reach", v.URL)
	}
	if !tokenPattern.MatchString(v.Token) {
		return "", fmt.Errorf("node token has an unexpected form")
	}
	quoted := BootValues{Image: v.Image, URL: yamlString(v.URL), Token: yamlString(v.Token)}
	var buf bytes.Buffer
	if err := boot.Execute(&buf, quoted); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// yamlString quotes a value as a YAML double-quoted scalar, which JSON is.
func yamlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
