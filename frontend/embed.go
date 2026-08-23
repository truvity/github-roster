// Package frontend embeds the built TypeScript UI (Vite/React) so the
// console can serve it under /app without a runtime build step. Regenerate
// dist/ with `just build-ui` after changing anything under src/.
package frontend

import "embed"

// Assets holds the built single-page app (dist/). Committed so CI and the
// image build need no Node toolchain — the same pattern as generated code.
//
//go:embed all:dist
var Assets embed.FS
