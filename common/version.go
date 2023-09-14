package common

import (
	"fmt"
)

// Version information set by link flags during build. We fall back to these sane
// default values when we build outside the Makefile context (e.g. go run, go build, or go test).
var (
	version   = "99.99.99"             // value from VERSION file
	buildDate = "1970-01-01T00:00:00Z" // output from `date -u +'%Y-%m-%dT%H:%M:%SZ'`
	gitCommit = ""                     // output from `git rev-parse HEAD`
	gitTag    = ""                     // output from `git describe --exact-match --tags HEAD` (if clean tree state)
)

// Version contains agent version information
type Version struct {
	Version   string
	BuildDate string
	GitCommit string
	GitTag    string
	GoVersion string
	Compiler  string
	Platform  string
}

func (v Version) String() string {
	return v.Version
}

// GetVersion returns the version information
func getVersion() Version {
	var versionStr string

	if gitCommit != "" && gitTag != "" {
		versionStr = gitTag
	} else {
		versionStr = "v" + version
		if len(gitCommit) >= 7 {
			versionStr += "+" + gitCommit[0:7]
		} else {
			versionStr += "+unknown"
		}
	}
	return Version{
		Version:   versionStr,
		BuildDate: buildDate,
		GitCommit: gitCommit,
		GitTag:    gitTag,
	}
}

func PrintVersion() {
	version := getVersion()
	fmt.Printf("agent: %s\n", version)
	fmt.Printf("  BuildDate: %s\n", version.BuildDate)
	fmt.Printf("  GitCommit: %s\n", version.GitCommit)
	if version.GitTag != "" {
		fmt.Printf("  GitTag: %s\n", version.GitTag)
	}
}
