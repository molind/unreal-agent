// Command unreal_web serves the local agent through a private browser UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/webchat"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "unreal_web:", err)
		os.Exit(1)
	}
}

func run() error {
	f := flag.NewFlagSet("unreal_web", flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:8097", "HTTP IP:port to listen on (e.g. 100.124.20.20:8097)")
	publicURL := f.String("public-url", "", "Tailscale Serve origin, e.g. https://machine.tailnet.ts.net")
	tailscaleUser := f.String("tailscale-user", "", "optional exact Tailscale login allowed without a token (trusted local Serve proxy)")
	state := f.String("state-directory", "", "web registry/token directory (default XDG state/unreal-agent/web)")
	values := make(map[string]*string)
	for _, name := range []string{"provider", "model", "reasoning-effort", "transport", "base-url", "max-attempts"} {
		values[name] = f.String(name, "", "override the matching unreal_chat setting")
	}
	f.Usage = func() {
		fmt.Fprintln(f.Output(), "Usage: unreal_web [flags] [workspace ...]\nFolders must be absolute paths. Use -listen IP:port to bind to a specific interface, or Tailscale Serve for HTTPS.")
		f.PrintDefaults()
	}
	if err := f.Parse(os.Args[1:]); err != nil {
		return err
	}
	if err := validateListen(*listen, *tailscaleUser); err != nil {
		return err
	}
	if *state == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if !filepath.IsAbs(base) {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			base = filepath.Join(home, ".local", "state")
		}
		*state = filepath.Join(base, "unreal-agent", "web")
	}
	if !filepath.IsAbs(*state) {
		return errors.New("-state-directory must be absolute")
	}
	origins := []string{"http://" + *listen}
	if *publicURL != "" {
		if !strings.HasPrefix(*publicURL, "https://") {
			return errors.New("-public-url must use HTTPS")
		}
		origins = append(origins, strings.TrimSuffix(*publicURL, "/"))
	}
	if *tailscaleUser != "" && *publicURL == "" {
		return errors.New("-tailscale-user requires -public-url")
	}
	var chatArgs []string
	f.Visit(func(v *flag.Flag) {
		if value := values[v.Name]; value != nil {
			chatArgs = append(chatArgs, "-"+v.Name, *value)
		}
	})
	s, err := webchat.New(webchat.Config{StateDirectory: *state, Origins: origins, TailscaleUser: *tailscaleUser, ChatArgs: chatArgs})
	if err != nil {
		return err
	}
	defer s.Close()
	for _, path := range f.Args() {
		if _, err := s.AddProject(path); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Println("Unreal web:", origins[len(origins)-1])
	fmt.Println("Sign in with the token stored in:", s.TokenPath())
	fmt.Println("Tools run with this user's local permissions. Closing the browser leaves work running.")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- server.Shutdown(shutdown) }()
	err = s.Close()
	if e := <-drained; e != nil {
		_ = server.Close()
		err = errors.Join(err, e)
	}
	return err
}

func validateListen(address, tailscaleUser string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback() && !ip.IsGlobalUnicast()) {
		return errors.New("-listen requires a specific unicast IP, such as 127.0.0.1 or your Tailscale IP; wildcard addresses are not supported")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return errors.New("invalid listen port")
	}
	if tailscaleUser != "" && !ip.IsLoopback() {
		return errors.New("-tailscale-user requires a loopback -listen address and trusted local Serve proxy; use token sign-in for direct IP access")
	}
	return nil
}
