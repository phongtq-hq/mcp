// Package migrations nhúng các file migration SQL (goose) vào binary.
package migrations

import "embed"

// FS chứa mọi file migration, theo thứ tự tên file.
//
//go:embed *.sql
var FS embed.FS
