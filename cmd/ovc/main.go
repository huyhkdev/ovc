package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"ovc/internal/cli"
)

func main() {
	err := cli.NewRoot().ExecuteContext(context.Background())
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	var ce *cli.CodedError
	if errors.As(err, &ce) {
		os.Exit(ce.Code)
	}
	os.Exit(cli.ExitError)
}
