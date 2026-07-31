package mirror

import "embed"

// assets contains the complete, local-only browser client.
//
//go:embed assets
var assets embed.FS
