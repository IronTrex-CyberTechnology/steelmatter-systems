package main

import (
	"runtime/debug"
	"testing"
)

// T43: smdemo run names its commit without -commit.
func TestDefaultCommit(t *testing.T) {
	const sha = "8c1ecd3a6f0e4b1c2d3e4f5a6b7c8d9e0f1a2b3c"
	vcs := func(kv ...string) *debug.BuildInfo {
		info := &debug.BuildInfo{}
		for i := 0; i < len(kv); i += 2 {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return info
	}
	for _, c := range []struct {
		name, set string
		info      *debug.BuildInfo
		want      string
	}{
		{"set at build time wins", sha, vcs("vcs.revision", "other", "vcs.modified", "true"), sha},
		{"from git", "", vcs("vcs.revision", sha, "vcs.modified", "false"), sha},
		{"from a tree with changes", "", vcs("vcs.revision", sha, "vcs.modified", "true"), sha + "-dirty"},
		{"no commit known", "", vcs(), "unknown"},
		{"no build info", "", nil, "unknown"},
	} {
		if got := defaultCommit(c.set, c.info); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
