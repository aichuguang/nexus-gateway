//go:build !linux && !windows

package traffic

import (
	"errors"
	"os"
)

func openSource(string) (*os.File, error) {
	return nil, errors.New("traffic collection requires Linux or Windows file identities")
}
func fileIdentity(*os.File) (string, error) {
	return "", errors.New("stable file identity unavailable")
}
