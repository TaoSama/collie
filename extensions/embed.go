// Package extensions holds Collie's extensions, one directory each, and the
// built-in catalog: every directory's collie-extension.json (README.md).
package extensions

import "embed"

// Manifests is every extension's collie-extension.json.
//
//go:embed */collie-extension.json
var Manifests embed.FS
