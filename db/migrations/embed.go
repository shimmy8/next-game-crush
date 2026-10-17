package migrations

import "embed"

// FS contains the immutable schema migrations shipped with the application.
//
//go:embed *.sql
var FS embed.FS
