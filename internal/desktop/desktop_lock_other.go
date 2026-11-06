//go:build !linux

package desktop

import "errors"

// Acquire on platforms without flock support fails closed: a gateway that
// proceeded without the inter-process lock could silently run a second
// instance against the same config directory.
func Acquire(path string) (*Lock, bool, error) {
	return nil, false, errors.New("desktop process lock currently requires Linux")
}

func (l *Lock) Close() error {
	return nil
}
