// Command siftstr is the Siftstr server and CLI.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Jolls/Siftstr/internal/auth"
	"github.com/Jolls/Siftstr/internal/config"
	"github.com/Jolls/Siftstr/internal/secret"
	"github.com/Jolls/Siftstr/internal/server"
	"github.com/Jolls/Siftstr/internal/store"
)

const usage = `usage:
  siftstr serve                                  run the server (default)
  siftstr healthcheck                            exit 0 if the local server answers /healthz (for Docker)
  siftstr user add [--admin] <username>          create a user; password is read from stdin
  siftstr user set-password <username>           reset a password; password is read from stdin`

func main() {
	if err := run(os.Args[1:], os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "siftstr:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve()
	case "healthcheck":
		return healthcheck(os.Getenv("SIFTSTR_LISTEN"))
	case "user":
		return userCmd(args, stdin)
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
}

// open loads config, the database and the auth service.
func open(ctx context.Context) (config.Config, *store.Store, *auth.Service, error) {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return cfg, nil, nil, err
	}
	master, err := secret.Load(cfg.DataDir, cfg.SecretKey)
	if err != nil {
		return cfg, nil, nil, err
	}
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return cfg, nil, nil, err
	}
	a, err := auth.New(st.DB(), secret.Derive(master, "csrf"))
	if err != nil {
		_ = st.Close()
		return cfg, nil, nil, err
	}
	return cfg, st, a, nil
}

func serve() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, st, a, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	created, err := a.Bootstrap(ctx, cfg.AdminUser, cfg.AdminPassword)
	if err != nil {
		return err
	}
	if created {
		log.Printf("created bootstrap admin %q", strings.ToLower(strings.TrimSpace(cfg.AdminUser)))
	}

	h, err := server.New(server.Deps{Ready: st.Ping, Auth: a, SecureCookies: cfg.SecureCookies()})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("listening on %s", cfg.Listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// healthcheck probes /healthz on the local listener. The runtime image has no
// shell or curl, so Docker's HEALTHCHECK runs this instead.
func healthcheck(listen string) error {
	if listen == "" {
		listen = ":8080"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("SIFTSTR_LISTEN: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func userCmd(args []string, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	sub, args := args[0], args[1:]

	fs := flag.NewFlagSet("user "+sub, flag.ContinueOnError)
	admin := fs.Bool("admin", false, "create the user as an admin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(usage)
	}
	username := fs.Arg(0)

	// The password comes from stdin so it never appears in the process list
	// or shell history.
	password, err := readPassword(stdin)
	if err != nil {
		return err
	}

	ctx := context.Background()
	_, st, a, err := open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "add":
		role := auth.RoleUser
		if *admin {
			role = auth.RoleAdmin
		}
		u, err := a.CreateUser(ctx, username, password, role)
		if err != nil {
			return err
		}
		fmt.Printf("created %s %q\n", u.Role, u.Username)
	case "set-password":
		if err := a.SetPassword(ctx, username, password); err != nil {
			return err
		}
		fmt.Printf("password updated for %q; existing sessions ended\n", strings.ToLower(strings.TrimSpace(username)))
	default:
		return fmt.Errorf("unknown user command %q\n%s", sub, usage)
	}
	return nil
}

func readPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", errors.New("no password on stdin (pipe it in, e.g. printf '%s' \"$PW\" | siftstr user add alice)")
	}
	return strings.TrimRight(line, "\r\n"), nil
}
