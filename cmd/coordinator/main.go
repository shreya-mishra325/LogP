package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"browsercompute.local/runtime/internal/identity"
	"browsercompute.local/runtime/internal/server"
)

const (
	operatorEnv   = "BROWSER_COMPUTE_OPERATOR_TOKEN"
	workerEnv     = "BROWSER_COMPUTE_WORKER_TOKEN"
	defaultListen = "127.0.0.1:8080"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, identity.Default()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage(w io.Writer, d identity.Display) {
	fmt.Fprintf(w, "%s — %s\n%s\nUsage: %s <serve|submit|status|result|cancel> [flags]\n", d.Name, d.Description, d.Tagline, d.CLI)
}

func run(ctx context.Context, args []string, out, errOut io.Writer, d identity.Display) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(out, d)
		return nil
	}
	cmd := args[0]
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.Usage = func() {
		fmt.Fprintf(errOut, "%s · Usage: %s %s [flags]\n", d.Name, d.CLI, cmd)
		flags.PrintDefaults()
	}
	var listen, address, file, job, destination string
	switch cmd {
	case "serve":
		flags.StringVar(&listen, "listen", defaultListen, "coordinator bind address")
	case "submit", "status", "result", "cancel":
		flags.StringVar(&address, "server", "http://"+defaultListen, "trusted coordinator origin")
		if cmd == "submit" {
			flags.StringVar(&file, "file", "", "job JSON path")
		} else {
			flags.StringVar(&job, "job", "", "job ID")
		}
		if cmd == "result" {
			flags.StringVar(&destination, "out", "", "new PNG output path")
		}
	default:
		return errors.New("unknown command; use --help")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if cmd == "serve" {
		return serve(ctx, listen, out, errOut, d)
	}
	return request(ctx, cmd, address, file, job, destination, out)
}

func credential(key string) (string, bool) {
	if value := os.Getenv(key); value != "" {
		return value, false
	}
	var data [32]byte
	rand.Read(data[:])
	return hex.EncodeToString(data[:]), true
}

func serve(ctx context.Context, address string, out, errOut io.Writer, display identity.Display) error {
	operator, newOperator := credential(operatorEnv)
	worker, newWorker := credential(workerEnv)
	if newOperator || newWorker {
		info, err := os.Stderr.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			return errors.New("generated credentials require an interactive terminal; set both credential environment variables for unattended use")
		}
	}
	app, err := server.New(ctx, operator, worker, display)
	if err != nil {
		return err
	}
	defer app.Close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	if newOperator {
		fmt.Fprintf(errOut, "Operator credential: %s\n", operator)
	}
	if newWorker {
		fmt.Fprintf(errOut, "Worker credential: %s\n", worker)
	}
	fmt.Fprintf(out, "Listening on %s\n", listener.Addr())
	if tcp, ok := listener.Addr().(*net.TCPAddr); ok && !tcp.IP.IsLoopback() {
		fmt.Fprintln(errOut, "HTTP is unencrypted. Use a trusted private LAN. Open http://<coordinator-LAN-IP>:<port>/worker on the participant device.")
	}
	srv := &http.Server{
		Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Serve(listener) }()
	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdown)
		if err != nil {
			_ = srv.Close()
		}
		<-stopped
		return err
	}
}

func request(ctx context.Context, cmd, address, file, job, destination string, out io.Writer) error {
	origin, err := url.Parse(address)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Hostname() == "" ||
		origin.User != nil || (origin.Path != "" && origin.Path != "/") || origin.ForceQuery || origin.RawQuery != "" || origin.Fragment != "" {
		return errors.New("--server must be a trusted HTTP(S) origin")
	}
	token := os.Getenv(operatorEnv)
	if token == "" {
		return errors.New("set " + operatorEnv)
	}
	method, path := http.MethodGet, "/api/v1/jobs"
	var body io.Reader
	if cmd == "submit" {
		if file == "" {
			return errors.New("--file is required")
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		if err != nil {
			return err
		}
		if len(data) > 64<<10 {
			return errors.New("job file exceeds 64 KiB")
		}
		body = strings.NewReader(string(data))
		method = http.MethodPost
	} else {
		if job == "" || strings.ContainsAny(job, "/\\") || job == "." || job == ".." {
			return errors.New("valid --job is required")
		}
		path += "/" + url.PathEscape(job)
		switch cmd {
		case "cancel":
			method = http.MethodPost
			path += "/cancel"
		case "result":
			if destination == "" {
				return errors.New("--out is required")
			}
			path += "/result"
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, origin.Scheme+"://"+origin.Host+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var detail struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&detail)
		return fmt.Errorf("coordinator returned %s %s", res.Status, detail.Error)
	}
	if cmd == "result" {
		return saveResult(destination, res)
	}
	_, err = io.Copy(out, res.Body)
	return err
}

func saveResult(path string, res *http.Response) error {
	if res.Header.Get("Content-Type") != "image/png" {
		return errors.New("expected PNG response")
	}
	// Read a complete bounded response before creating the destination.
	const maxPNG = 20 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, maxPNG+1))
	if err != nil {
		return err
	}
	if len(data) > maxPNG {
		return errors.New("PNG exceeds response limit")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}
