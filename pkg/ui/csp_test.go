package ui

import (
	"io/fs"
	"strings"
	"testing"
)

// The CSP carries no unsafe-inline for script-src, so an inline <script>
// in a template is silently dead in the browser — exactly what happened
// to the mapping form's prefill after the v0.15.0 hardening. Every
// script must load from /static/ (StaticFS).
func TestTemplatesCarryNoInlineScripts(t *testing.T) {
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}

		body := string(data)
		for rest := body; ; {
			i := strings.Index(rest, "<script")
			if i < 0 {
				break
			}

			tag := rest[i:]
			if end := strings.Index(tag, ">"); end >= 0 {
				tag = tag[:end]
			}

			if !strings.Contains(tag, "src=") {
				t.Errorf("%s: inline <script> without src — blocked by the CSP, move it to static/", path)
			}

			rest = rest[i+len("<script"):]
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
