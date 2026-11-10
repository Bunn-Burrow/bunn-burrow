package guestagent

import (
	"errors"
	"fmt"
	"log"
	"syscall"
)

func ReapChildren() error {
	for {
		var status syscall.WaitStatus

		pid, err := syscall.Wait4(
			-1,
			&status,
			syscall.WNOHANG,
			nil,
		)

		if err != nil {
			switch {
			case errors.Is(err, syscall.EINTR):
				continue
			case errors.Is(err, syscall.ECHILD):
				return nil
			default:
				return fmt.Errorf("wait4: %w", err)
			}
		}

		if pid == 0 {
			return nil
		}

		log.Printf(
			"reaped child pid=%d status=%v",
			pid,
			status,
		)
	}
}
