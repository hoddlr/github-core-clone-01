// Package main: entrypoint and the entry <-> env lifecycle.
//
//  1. Bind an ephemeral port (listen(0)). Never 8080: the env owns it.
//  2. POST {"port": N} to $MAKEFUN_ENV_URL/register (first register wins).
//  3. Poll GET $MAKEFUN_ENV_URL/done until {"done": true}.
//  4. Close the server and exit 0.
//
// The forge serves from this one process; no clock or unseeded randomness
// is read anywhere on a request path (see store.go).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// requestTimeout bounds one register/done request before it is retried.
const requestTimeout = 30 * time.Second

// retryDelay is the wait after attempt failed requests: exponential from
// 250 ms, capped at 2 s.
func retryDelay(attempt int) time.Duration {
	d := 250 * time.Millisecond
	for i := 0; i < attempt && i < 3; i++ {
		d *= 2
	}
	if d > 2*time.Second {
		return 2 * time.Second
	}
	return d
}

// register POSTs the bound port until the env accepts it. 200 is the normal
// answer; 409 means a first register already won. 400/405 are contract
// errors and end the program; anything else is retried.
func register(envURL string, port int, client *http.Client) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodPost, envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusConflict {
				return nil
			}
			if res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusMethodNotAllowed {
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done": true}; it never
// returns before that.
func waitForDone(envURL string, client *http.Client) {
	for attempt := 0; ; attempt++ {
		func() {
			req, err := http.NewRequest(http.MethodGet, envURL+"/done", nil)
			if err != nil {
				return
			}
			res, err := client.Do(req)
			if err != nil {
				return
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				return
			}
			var decoded struct {
				Done bool `json:"done"`
			}
			if json.NewDecoder(res.Body).Decode(&decoded) == nil && decoded.Done {
				attempt = -1 // signal completion via the outer check below
			}
		}()
		if attempt < 0 {
			return
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[forge] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}

	store := NewStore(
		os.Getenv("MAKEFUN_SHOP_TOKEN"),
		os.Getenv("MAKEFUN_TRIAL_NOW"),
		os.Getenv("MAKEFUN_TRIAL_SEED"),
	)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	server := &http.Server{
		Handler:           store,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		_ = server.Serve(listener)
	}()

	client := &http.Client{Timeout: requestTimeout}
	if err := register(envURL, port, client); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] %v\n", err)
		os.Exit(1)
	}
	waitForDone(envURL, client)
	_ = server.Close()
	os.Exit(0)
}
