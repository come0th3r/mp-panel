//go:build !linux

package agent

import "errors"

func diskSpace(path string) (uint64, uint64, error) {
	return 0, 0, errors.New("agent runtime requires Linux")
}
