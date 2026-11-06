package buildinfo

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	cases := []struct {
		name string
		info debug.BuildInfo
		want string
	}{
		{"nothing", debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev"},
		{"module version", debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, "v1.2.3"},
		{"revision", debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef"}}}, "dev+0123456"},
		{"dirty", debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"}, {Key: "vcs.modified", Value: "true"}}}, "dev+0123456-dirty"},
	}
	for _, c := range cases {
		if got := fromBuildInfo(&c.info); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestVersionWins(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = "1.2.3+sha-abc1234"
	if got := String(); got != Version {
		t.Fatalf("got %q", got)
	}
	if ua := UserAgent("worker"); !strings.HasPrefix(ua, "jellyfinner-worker/1.2.3+sha-abc1234 (") {
		t.Fatalf("user agent %q", ua)
	}
}
