// Package docs embeds the two manuals the console ships (built by each
// manual's build.sh from its parts), so the AI Developer can read them when
// it explains the platform (internal/manuals).
package docs

import _ "embed"

//go:embed developer-manual/manual.html
var DeveloperManual string

//go:embed formulas-manual/manual.html
var FormulasManual string
