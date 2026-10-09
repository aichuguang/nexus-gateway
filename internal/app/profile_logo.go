package app

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"
)

const (
	profileLogoMaxBytes     = 256 << 10
	profileLogoMaxDimension = 2048
	profileLogoFormatError  = "Logo \u4ec5\u652f\u6301 PNG\u3001JPEG \u6216 GIF \u56fe\u7247\uff0c\u8bf7\u4e0a\u4f20\u672c\u5730\u56fe\u7247"
	profileLogoSizeError    = "Logo \u56fe\u7247\u4e0d\u80fd\u8d85\u8fc7 256 KiB"
	profileLogoInvalidError = "Logo \u56fe\u7247\u5185\u5bb9\u65e0\u6548\u6216\u5df2\u635f\u574f\uff0c\u8bf7\u91cd\u65b0\u9009\u62e9"
	profileLogoBoundsError  = "Logo \u56fe\u7247\u7684\u5bbd\u548c\u9ad8\u987b\u4e3a 1 \u81f3 2048 \u50cf\u7d20"
)

// Keep branding self-contained in profile.json and the existing image CSP.
// Arbitrary URLs and SVG are intentionally not accepted as public login data.
func validateProfileLogo(logo string) error {
	if logo == "" {
		return nil
	}
	header, encoded, ok := strings.Cut(logo, ",")
	format := ""
	switch header {
	case "data:image/png;base64":
		format = "png"
	case "data:image/jpeg;base64":
		format = "jpeg"
	case "data:image/gif;base64":
		format = "gif"
	default:
		return errors.New(profileLogoFormatError)
	}
	if !ok || encoded == "" {
		return errors.New(profileLogoInvalidError)
	}
	if len(encoded) > base64.StdEncoding.EncodedLen(profileLogoMaxBytes) {
		return errors.New(profileLogoSizeError)
	}
	if strings.ContainsAny(encoded, " \t\r\n") {
		return errors.New(profileLogoInvalidError)
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return errors.New(profileLogoInvalidError)
	}
	if len(data) > profileLogoMaxBytes {
		return errors.New(profileLogoSizeError)
	}
	config, actualFormat, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || actualFormat != format {
		return errors.New(profileLogoInvalidError)
	}
	if config.Width < 1 || config.Height < 1 || config.Width > profileLogoMaxDimension || config.Height > profileLogoMaxDimension {
		return errors.New(profileLogoBoundsError)
	}
	// Check dimensions before allocating pixels, then reject truncated or corrupt
	// image bodies rather than trusting the data URL's MIME type or header alone.
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return errors.New(profileLogoInvalidError)
	}
	return nil
}
