package buildinfo

import (
	"runtime"
	"strings"
)

// These values are replaced by release builds with -ldflags -X.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	Target    string `json:"target"`
}

func Current() Info {
	return Info{
		Version:   strings.TrimSpace(Version),
		Commit:    strings.TrimSpace(Commit),
		BuildDate: strings.TrimSpace(BuildDate),
		Target:    runtime.GOOS + "/" + runtime.GOARCH,
	}
}
