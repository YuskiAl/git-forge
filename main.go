// git-forge entrypoint. One process: bind a port, register it with the
// trial environment, serve until the suite reports done, exit 0.
// The shop token is read from the environment the trial provides.
// Port of main.ts.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"
)

func main() {
	if os.Getenv("FORGE_SELFTEST") != "" {
		os.Exit(runSelfTest(os.Getenv("FORGE_SELFTEST")))
	}
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(-1)
	debug.SetMemoryLimit(768 << 20)
	startTimeCache()

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	shopToken := os.Getenv("MAKEFUN_SHOP_TOKEN")
	store := NewStore()
	srv := &httpServer{store: store, token: shopToken}

	var ln net.Listener
	var err error
	var port int
	if envURL == "" {
		// Local development: no trial environment; serve on FORGE_PORT
		// (default 18080) so the kit runner can reach us.
		p := os.Getenv("FORGE_PORT")
		if p == "" {
			p = "18080"
		}
		ln, err = net.Listen("tcp", "127.0.0.1:"+p)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		port = ln.Addr().(*net.TCPAddr).Port
		_ = port
		fmt.Fprintf(os.Stderr, "forge listening on 127.0.0.1:%d\n", port)
		srv.Serve(ln)
		return
	}

	ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port = ln.Addr().(*net.TCPAddr).Port
	go srv.Serve(ln)

	client := &http.Client{Timeout: 30 * time.Second}
	postJSON := func(url string, payload interface{}) (interface{}, error) {
		body, _ := json.Marshal(payload)
		resp, err := client.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var out interface{}
		if err := json.Unmarshal(data, &out); err != nil {
			return string(data), nil
		}
		return out, nil
	}
	getJSON := func(url string) (interface{}, error) {
		resp, err := client.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var out interface{}
		if err := json.Unmarshal(data, &out); err != nil {
			return string(data), nil
		}
		return out, nil
	}

	if _, err := postJSON(envURL+"/register", map[string]interface{}{"port": port}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for {
		done, err := getJSON(envURL + "/done")
		if err == nil {
			if m, ok := done.(map[string]interface{}); ok {
				if d, ok := m["done"].(bool); ok && d {
					break
				}
			}
		}
		time.Sleep(1000 * time.Millisecond)
	}
	_ = ln.Close()
	os.Exit(0)
}

// startTimeCache keeps a 250 ms cached RFC3339 timestamp string, as the
// ricoxyz time cache does. (Deterministic store timestamps do not use it;
// it is kept for parity and future use.)
var cachedTimeStr atomicString

type atomicString struct {
	v atomic.Value
}

func (a *atomicString) Store(s string) { a.v.Store(s) }
func (a *atomicString) Load() string {
	if v := a.v.Load(); v != nil {
		return v.(string)
	}
	return ""
}

func startTimeCache() {
	cachedTimeStr.Store(time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	go func() {
		for range time.Tick(250 * time.Millisecond) {
			cachedTimeStr.Store(time.Now().UTC().Format("2006-01-02T15:04:05Z"))
		}
	}()
}

var _ = strconv.Itoa
