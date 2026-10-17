package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata"

	_ "github.com/paymux/paymux/connectors/razorpay" // registers the razorpay connector
	"github.com/paymux/paymux/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
