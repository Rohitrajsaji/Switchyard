// Package migrations embeds numbered SQL so the binary and schema release together.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed *.sql
var Files embed.FS

func Latest() string {
	names, err := fs.Glob(Files, "*.sql")
	if err != nil || len(names) == 0 {
		panic("embedded migrations missing")
	}
	return names[len(names)-1]
}
