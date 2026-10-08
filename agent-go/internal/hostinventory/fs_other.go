//go:build !linux

package hostinventory

import "errors"

func (local) FS(string) (map[string]uint64, error) { return nil, errors.New("unsupported platform") }
