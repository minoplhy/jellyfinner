// Package buildinfo names the build a program was made from, so every node
// can report it to the orchestrator.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is set at link time, for example by the image build:
//
//	go build -ldflags "-X jellyfinner/internal/buildinfo.Version=1.2.3+sha-abc1234"
var Version = ""

// String returns Version, or else the VCS revision Go recorded, or "dev".
func String() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	return fromBuildInfo(info)
}

func fromBuildInfo(info *debug.BuildInfo) string {
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		return "dev"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "-dirty"
	}
	return "dev+" + rev
}

// UserAgent is what a node sends with each control API call.
func UserAgent(role string) string {
	return fmt.Sprintf("jellyfinner-%s/%s (%s/%s; %s)", role, String(), runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// Line is the one-line description printed by each program's version command.
func Line(program string) string {
	return fmt.Sprintf("%s %s %s/%s %s", program, String(), runtime.GOOS, runtime.GOARCH, runtime.Version())
}
