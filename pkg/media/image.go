package media

import "strings"

// IsViewableImage reports whether mime names a picture a model can be shown.
// CAD drawings are registered under image/ (filetype sniffs a DWG as
// image/vnd.dwg), and every provider refuses them as an invalid image. Other
// formats stay: a model the vision model hands off to may read them.
// (seucaranguejo fork)
func IsViewableImage(mime string) bool {
	switch mime {
	case "image/vnd.dwg", "image/x-dwg", "image/vnd.dxf", "image/x-dxf":
		return false
	}
	return strings.HasPrefix(mime, "image/")
}
