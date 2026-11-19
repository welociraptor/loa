package main

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// The flake sets these at link time. A plain `go build` from a git checkout
// leaves them empty, so the build info Go stamps fills them in instead.
var (
	version string
	commit  string
)

// versionLine names the release and the commit the binary was built from.
func versionLine() string {
	v, c := version, commit
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "" {
			v = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if c == "" {
					c = shortCommit(setting.Value)
				}
			case "vcs.modified":
				if c != "" && setting.Value == "true" && !strings.HasSuffix(c, "-dirty") {
					c += "-dirty"
				}
			}
		}
	}
	if v == "" || v == "(devel)" {
		v = "dev"
	}
	if c == "" {
		c = "dev"
	}
	return fmt.Sprintf("loa %s %s", v, c)
}

// shortCommit keeps a revision greppable without printing all forty hex
// characters.
func shortCommit(rev string) string {
	if len(rev) > 9 {
		return rev[:9]
	}
	return rev
}
