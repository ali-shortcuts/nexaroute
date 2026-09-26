//go:build !linux

package main

import (
	"fmt"
	"os"
)

func lockInstance(path string) (*os.File, error) {
	return nil, fmt.Errorf("this launcher currently supports Linux only")
}
