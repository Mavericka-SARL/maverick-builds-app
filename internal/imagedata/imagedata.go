// Package imagedata validates the small images the platform stores inline,
// as base64 data URLs, rather than as objects in the blob store: a tenant's
// logo and favicon (white-labelling) and a dashboard's image widgets.
//
// Inline is the right shape for these: they are small, there is at most a
// handful per model, and — the deciding reason — a data URL travels with
// the row. A dashboard exported as a package, copied into a new revision or
// imported into another tenant keeps its pictures with no second store to
// keep in step, no orphan sweep and no signed URLs. Anything large or
// numerous belongs in pkg/objectstore instead.
//
// SVG is allowed because diagrams are the most useful thing a dashboard can
// show; it is safe only because every renderer displays these through an
// <img> element, where a browser runs no script the file may contain.
// Rendering one inline, or serving it from the platform's own origin as a
// document, would not be safe.
package imagedata

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// Byte limits, applied to the decoded image.
const (
	MaxLogoBytes    = 256 * 1024
	MaxFaviconBytes = 32 * 1024
	// MaxWidgetBytes is a dashboard image. Larger than a logo because a
	// screenshot or a diagram is the point of it, and small enough that a
	// dashboard stays a row rather than a payload.
	MaxWidgetBytes = 512 * 1024
)

// allowedTypes are what an <img> element shows everywhere.
var allowedTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/svg+xml": true,
	"image/webp": true, "image/gif": true, "image/x-icon": true, "image/vnd.microsoft.icon": true,
}

// Validate accepts an empty value, or a base64 data URL of an allowed image
// type whose decoded size is within limit. what names the field in the
// error, which is shown to the person who chose the file.
func Validate(what, dataURL string, limit int) error {
	if dataURL == "" {
		return nil
	}
	if !strings.HasPrefix(dataURL, "data:") {
		return fmt.Errorf("%s must be an image data URL", what)
	}
	meta, payload, ok := strings.Cut(dataURL[len("data:"):], ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return fmt.Errorf("%s must be a base64 image data URL", what)
	}
	if typ := strings.TrimSuffix(meta, ";base64"); !allowedTypes[typ] {
		return fmt.Errorf("%s must be a PNG, JPEG, SVG, WebP, GIF or ICO image", what)
	}
	if n := base64.StdEncoding.DecodedLen(len(payload)); n > limit {
		return fmt.Errorf("%s must be at most %d KB", what, limit/1024)
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return fmt.Errorf("%s is not valid base64", what)
	}
	return nil
}
