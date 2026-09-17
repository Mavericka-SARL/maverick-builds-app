// Package migrations embeds all SQL migration files so they can be
// applied at runtime by any service or tool that imports this package.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
