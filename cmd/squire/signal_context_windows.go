//go:build windows

package main

import (
	"context"
	"os"
	"os/signal"
)

func rootContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
