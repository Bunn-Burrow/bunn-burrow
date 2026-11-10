package guestagent

import (
	"os"
	"os/signal"
	"syscall"
)

func NotifySignals() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 32)

	signal.Notify(
		ch,
		syscall.SIGTERM,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGCHLD,
	)

	return ch, func() {
		signal.Stop(ch)
	}
}
