//go:build !linux

package engine

import (
	"fmt"
	"os"
)

func proxyQuitSignal() os.Signal { return os.Interrupt }

func inspectNginxMaster(int, string, string) (bool, error) {
	return false, fmt.Errorf("managed nginx process verification requires Linux")
}
