//go:build !linux

package at

import "context"

// configureSerialNative is only used by the Linux implementation. The
// non-Linux build keeps the existing stty path in parser.go.
func configureSerialNative(context.Context, string, int) error {
	return nil
}
