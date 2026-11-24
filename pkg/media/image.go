package media

import "strings"

// IsViewableImage reports whether mime names a picture a model can be shown.
// AutoCAD drawings are registered (and sniffed by filetype) as image/vnd.dwg,
// and every provider refuses them as an invalid image. (seucaranguejo fork)
func IsViewableImage(mime string) bool {
	return strings.HasPrefix(mime, "image/") && mime != "image/vnd.dwg"
}
