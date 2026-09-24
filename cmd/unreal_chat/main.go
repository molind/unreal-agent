// Command unreal_chat runs an interactive local coding chat.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/cmd/internal/chat"
)

func main() { os.Exit(run()) }
func run() int {
	// Return broken-pipe write errors through Run so its defers join tools.
	brokenPipe := make(chan os.Signal, 1)
	signal.Notify(brokenPipe, unix.SIGPIPE)
	defer signal.Stop(brokenPipe)
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	files, closeFiles, err := chatStdio()
	if err == nil {
		err = func() (result error) {
			defer func() { result = errors.Join(result, closeFiles()) }()
			return chat.Run(context.Background(), os.Args[1:], os.Getenv, files[0], files[1], files[2], interrupts, agentrunner.DefaultProviders())
		}()
	}
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintf(os.Stderr, "unreal_chat: %v\n", err)
	return 1
}
