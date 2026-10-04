// Package migrations embeds numbered SQL so the binary and schema release together.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS
