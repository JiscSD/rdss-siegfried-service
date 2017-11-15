package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/JiscRDSS/rdss-siegfried-service/internal/group"
)

func usage() {
	fmt.Fprintf(os.Stderr, "USAGE\n")
}

func main() {
	var (
		addr = flag.String("addr", ":8080", "tcp network address")
		sf   = flag.String("sf", "/sf", "sf binary")
		home = flag.String("home", "/siegfried", "siegfried data diretory")
	)
	flag.Parse()

	if len(os.Args) < 1 {
		usage()
		os.Exit(1)
	}

	if err := run(*addr, *sf, *home); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(addr, sf, home string) error {
	// Bind listener.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// Start main goroutines.
	var g group.Group
	{
		// Run `siegfried -fpr`.
		cancel := make(chan struct{})
		g.Add(func() error {
			return siegfried(cancel, sf, home)
		}, func(error) {
			close(cancel)
		})
	}
	{

		// Run the HTTP API.
		g.Add(func() error {
			mux := http.NewServeMux()
			mux.HandleFunc("/", identifyHandler)
			return http.Serve(ln, mux)
		}, func(error) {
			ln.Close()
		})
	}
	{
		// Listen to system signals.
		cancel := make(chan struct{})
		g.Add(func() error {
			return interrupt(cancel)
		}, func(error) {
			close(cancel)
		})
	}
	return g.Run()
}

// interrupt waits until a signal is received or the cancel channel is closed.
func interrupt(cancel <-chan struct{}) error {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-c:
		return fmt.Errorf("received signal %s", sig)
	case <-cancel:
		return errors.New("canceled")
	}
}

// siegfried
func siegfried(cancel <-chan struct{}, sf, home string) error {
	cmd := exec.Command(sf, "-home", home, "-fpr")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	sout, err := ioutil.ReadAll(stdout)
	if err != nil {
		return err
	}

	serr, err := ioutil.ReadAll(stderr)
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			fmt.Println(string(sout))
			fmt.Println(string(serr))
		}
		return err
	case <-cancel:
		return cmd.Process.Kill()
	}
}

// identifyHandler is the main request handler used to identify a file.
func identifyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		handleErr(w, http.StatusMethodNotAllowed, fmt.Errorf("only GET is supported"))
		return
	}
	path := r.URL.Path
	if len(path) < 2 {
		handleErr(w, http.StatusNotFound, fmt.Errorf("path is empty"))
		return
	}
	loc, err := base64.URLEncoding.DecodeString(path[1:])
	if err != nil {
		handleErr(w, http.StatusInternalServerError, err)
		return
	}
	puid, err := identify(r.Context(), loc)
	if err != nil {
		handleErr(w, http.StatusInternalServerError, err)
		return
	}
	res := struct {
		PUID string `json:"puid"`
	}{
		PUID: puid,
	}
	blob, err := json.Marshal(res)
	if err != nil {
		handleErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json: charset=utf-8")
	w.Write(blob)
}

// handlerErr updates the response with error details.
func handleErr(w http.ResponseWriter, status int, e error) {
	w.WriteHeader(status)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, fmt.Sprintf("server error; got %v\n", e))
}

// identify connects to Siegfried's UNIX socket and writes the path of the file
// that needs to be identified. It takes a context to control cancelation and
// it creates a child context with a timeout hard-coded to 10 minutes.
func identify(ctx context.Context, path []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute*10)
	defer cancel()

	const addr = "/tmp/siegfried"
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", addr)
	if err != nil {
		return "", err
	}
	defer c.Close()

	// Send query and read
	c.Write(path)
	buf := make([]byte, 1024)
	n, err := c.Read(buf[:])
	if err != nil {
		return "", err
	}

	return string(buf[0:n]), nil
}
