// Package version carries the build stamp injected at release time.
//
// The git tag is the single version authority: goreleaser injects the tag
// here, and the Helm chart's own version field is a placeholder stamped from
// the same tag at package time. Nothing in the tree records a version by hand.
package version

// Info is the build stamp.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
}

// String renders the stamp for humans: "1.2.3 (abc1234)", or just the version
// when the commit is unknown (a `go build` outside the release pipeline).
func (i Info) String() string {
	if i.Commit == "" {
		return i.Version
	}

	commit := i.Commit
	if len(commit) > 7 {
		commit = commit[:7]
	}

	return i.Version + " (" + commit + ")"
}
