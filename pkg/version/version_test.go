package version_test

import (
	"testing"

	"github.com/truvity/github-roster/pkg/version"
)

func TestInfoString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		info version.Info
		want string
	}{
		{"no commit", version.Info{Version: "dev"}, "dev"},
		{"short commit", version.Info{Version: "1.0.0", Commit: "abc"}, "1.0.0 (abc)"},
		{"full sha is abbreviated", version.Info{Version: "1.0.0", Commit: "0123456789abcdef"}, "1.0.0 (0123456)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.info.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
