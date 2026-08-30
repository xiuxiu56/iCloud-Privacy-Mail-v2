package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
)

var (
	Version = "2.2.2"
	Commit  = "unknown"
	BuiltAt = ""
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"built_at"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Current 返回当前二进制的版本、提交和构建环境。
func Current() Info {
	version := strings.TrimSpace(Version)
	commit := strings.TrimSpace(Commit)
	builtAt := strings.TrimSpace(BuiltAt)

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "" || commit == "unknown" {
					commit = strings.TrimSpace(setting.Value)
				}
			case "vcs.time":
				if builtAt == "" {
					builtAt = strings.TrimSpace(setting.Value)
				}
			}
		}
	}

	if version == "" {
		version = "2.2.2"
	}
	if commit == "" {
		commit = "unknown"
	}

	return Info{
		Version: version,
		Commit:  commit,
		BuiltAt: builtAt,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
}
