//go:build !linux && !darwin

package snapshotstore

import "errors"

func availableBytes(string) (uint64, error) {
	return 0, errors.New("replication capacity preflight requires Linux or macOS")
}
