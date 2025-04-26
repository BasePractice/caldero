package services

import (
	"context"
	"os/signal"
	"syscall"
)

type ExitHandler func(context.Context)

func ExitHandle(handler ExitHandler) context.Context {
	parent := context.Background()
	go func() {
		ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		<-ctx.Done()
		handler(ctx)
	}()
	return parent
}
