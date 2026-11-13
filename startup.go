// startup.go — restarts without a refused connection.
//
// Three pieces (docs/restart.md):
//
//  1. The listening socket outlives the process. Under systemd the socket
//     unit (umfeld-at.socket) owns :PORT and hands it over as fd 3
//     (LISTEN_FDS); while the service restarts the kernel keeps accepting
//     into the backlog, so a client sees a slower answer, never a refusal.
//     Without a socket unit we still bind before any init work.
//  2. Readiness is explicit: READY=1 goes to systemd (Type=notify) only when
//     the accept loop runs, so `systemctl restart` returns when the service
//     answers — and fails when it does not (scripts/deploy.sh rolls back).
//  3. SIGTERM drains: in-flight requests finish (startupDrain), then exit.
//
// A watchdog ping (WATCHDOG_USEC) proves the accept loop still answers; a
// wedged process is restarted by systemd instead of sitting on the socket.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// startupDrain — how long in-flight requests get after SIGTERM. Long exports
// are cut; the unit's TimeoutStopSec must stay above it.
const startupDrain = 10 * time.Second

var startupT0 = time.Now()

// startupListen — the systemd-passed socket when there is one, else a fresh
// bind. Called first thing in server mode so nothing is refused during init.
func startupListen(port string) (net.Listener, bool, error) {
	if pid, _ := strconv.Atoi(os.Getenv("LISTEN_PID")); pid == os.Getpid() {
		if n, _ := strconv.Atoi(os.Getenv("LISTEN_FDS")); n >= 1 {
			const fd = 3 // SD_LISTEN_FDS_START
			syscall.CloseOnExec(fd)
			f := os.NewFile(fd, "systemd-socket")
			ln, err := net.FileListener(f)
			_ = f.Close() // FileListener dup'ed it
			os.Unsetenv("LISTEN_PID")
			os.Unsetenv("LISTEN_FDS")
			os.Unsetenv("LISTEN_FDNAMES")
			if err == nil {
				return ln, true, nil
			}
			log.Printf("⚠️  systemd socket unusable (%v) — binding :%s", err, port)
		}
	}
	ln, err := net.Listen("tcp", ":"+port)
	return ln, false, err
}

// sdNotify — one datagram to $NOTIFY_SOCKET; a no-op outside systemd.
func sdNotify(state string) {
	name := os.Getenv("NOTIFY_SOCKET")
	if name == "" {
		return
	}
	if name[0] == '@' { // abstract namespace
		name = "\x00" + name[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return
	}
	_, _ = c.Write([]byte(state))
	_ = c.Close()
}

// startupStage runs one init step and logs it when it is slow enough to
// matter — the restart budget is read off these lines.
func startupStage(name string, fn func()) {
	t := time.Now()
	fn()
	if d := time.Since(t); d >= 100*time.Millisecond {
		log.Printf("⏱  startup: %s %s", name, d.Round(time.Millisecond))
	}
}

// startupParallel runs independent init steps side by side and waits.
func startupParallel(steps map[string]func()) {
	var wg sync.WaitGroup
	for name, fn := range steps {
		wg.Add(1)
		go func() { defer wg.Done(); startupStage(name, fn) }()
	}
	wg.Wait()
}

// startupServe — accept on ln, tell systemd we are ready, drain on
// SIGTERM/SIGINT. Returns when the server has stopped.
func startupServe(srv *http.Server, ln net.Listener, afterReady func()) error {
	nonce := startupNonce()
	inner := srv.Handler
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(startupPingHeader) == nonce { // watchdog self-ping: no tier, no log, no gate
			w.WriteHeader(http.StatusNoContent)
			return
		}
		inner.ServeHTTP(w, r)
	})

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	sdNotify("READY=1")
	log.Printf("✅ ready in %s (pid %d)", time.Since(startupT0).Round(time.Millisecond), os.Getpid())
	go startupWatchdog(ln.Addr(), nonce)
	if afterReady != nil {
		go afterReady()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case err := <-errc:
		return err
	case s := <-sig:
		sdNotify("STOPPING=1")
		t := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), startupDrain)
		defer cancel()
		err := srv.Shutdown(ctx)
		if err != nil {
			_ = srv.Close()
		}
		log.Printf("🛑 %v: drained in %s (%v)", s, time.Since(t).Round(time.Millisecond), err)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil // drain budget spent: requests cut, still a clean stop
		}
		return err
	}
}

const startupPingHeader = "X-Umfeld-Ping"

func startupNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// startupWatchdog — when the unit sets WatchdogSec, ping systemd at a third
// of the interval, but only after our own accept loop answered a loopback
// request. A process that no longer serves stops pinging and is restarted.
func startupWatchdog(addr net.Addr, nonce string) {
	usec, _ := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if usec <= 0 {
		return
	}
	if pid, err := strconv.Atoi(os.Getenv("WATCHDOG_PID")); err == nil && pid != os.Getpid() {
		return
	}
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return
	}
	every := time.Duration(usec) * time.Microsecond / 3
	// plain transport on purpose: loopback only, never a peer (invariant 15 does not apply)
	cl := &http.Client{Timeout: every, Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://127.0.0.1:" + port + "/"
	for {
		req, _ := http.NewRequest(http.MethodHead, url, nil)
		req.Header.Set(startupPingHeader, nonce)
		if resp, err := cl.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				sdNotify("WATCHDOG=1")
			}
		} else {
			log.Printf("⚠️  watchdog self-ping failed: %v", err)
		}
		time.Sleep(every)
	}
}
