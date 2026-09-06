//go:build linux

package at

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// configureSerialNative uses termios directly on Linux. It avoids invoking
// stty for every candidate during USB enumeration, which is important on
// kernels where an unplugged USB interface can leave stty waiting forever.
func configureSerialNative(ctx context.Context, path string, baud int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open serial port for termios %s: %w", path, err)
	}
	defer file.Close()
	termios, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	if err != nil {
		return fmt.Errorf("read termios for serial port %s: %w", path, err)
	}
	termios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	termios.Oflag &^= unix.OPOST
	termios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	termios.Cflag &^= unix.CSIZE | unix.PARENB
	termios.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL
	speed := uint32(unix.B9600)
	if baud == 115200 {
		speed = uint32(unix.B115200)
	}
	termios.Ispeed = speed
	termios.Ospeed = speed
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(int(file.Fd()), unix.TCSETS, termios); err != nil {
		return fmt.Errorf("write termios for serial port %s: %w", path, err)
	}
	return nil
}
