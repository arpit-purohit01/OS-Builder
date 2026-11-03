// Package sdnotify implements the systemd notify protocol (READY, WATCHDOG,
// STATUS) without cgo or libsystemd.
package sdnotify

import (
	"net"
	"os"
	"strconv"
	"time"
)

// Notify sends state to $NOTIFY_SOCKET. Returns false if not under systemd.
func Notify(state string) (bool, error) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return false, nil
	}
	if sock[0] == '@' {
		sock = "\x00" + sock[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return false, err
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err == nil, err
}

// WatchdogInterval returns half the configured watchdog period (0 if none).
func WatchdogInterval() time.Duration {
	us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || us <= 0 {
		return 0
	}
	if p := os.Getenv("WATCHDOG_PID"); p != "" && p != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(us) * time.Microsecond / 2
}
