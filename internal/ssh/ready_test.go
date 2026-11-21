package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestReadVersionAcceptsARealIdentification pins the happy path against the exact
// line format OpenSSH emits (terminated by CRLF).
func TestReadVersionAcceptsARealIdentification(t *testing.T) {
	sv, err := ReadVersion(strings.NewReader("SSH-2.0-OpenSSH_9.9\r\n"))
	if err != nil {
		t.Fatalf("ReadVersion: %v", err)
	}
	if sv.Raw != "SSH-2.0-OpenSSH_9.9" {
		t.Errorf("Raw = %q", sv.Raw)
	}
	if sv.Protocol != "2.0" {
		t.Errorf("Protocol = %q", sv.Protocol)
	}
	if sv.Implementation != "OpenSSH_9.9" {
		t.Errorf("Implementation = %q", sv.Implementation)
	}
	if len(sv.BannerLines) != 0 {
		t.Errorf("unexpected banner lines: %v", sv.BannerLines)
	}
}

// TestReadVersionHandlesPreBannerLines covers RFC 4253's allowance for arbitrary
// pre-identification text. A guest behind a login banner or a TLS-terminating
// forwarder can emit text first, and giving up there would be a false negative.
func TestReadVersionHandlesPreBannerLines(t *testing.T) {
	in := "some gateway hello\r\nanother line\r\nSSH-2.0-OpenSSH_8.9 Fedora-22.fc38\r\n"
	sv, err := ReadVersion(strings.NewReader(in))
	if err != nil {
		t.Fatalf("pre-banner text must be tolerated: %v", err)
	}
	if sv.Implementation != "OpenSSH_8.9 Fedora-22.fc38" {
		t.Errorf("Implementation = %q", sv.Implementation)
	}
	if len(sv.BannerLines) != 2 || sv.BannerLines[0] != "some gateway hello" {
		t.Errorf("BannerLines = %v", sv.BannerLines)
	}
	// An implementation string may legally contain spaces, so the whole remainder
	// must survive rather than just the first word.
}

// TestReadVersionClassifiesNonSSHListeners is the point of checking the banner at
// all: a TCP connection succeeding proves nothing. While a guest boots, the
// forwarded port can be closed, can answer with another service, or can be
// accepted by a proxy that never speaks SSH.
func TestReadVersionClassifiesNonSSHListeners(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  error
		msg   string
	}{
		{"http", "HTTP/1.1 400 Bad Request\r\n\r\n", ErrNotSSH, "no SSH identification"},
		{"banner then close", "Welcome to the lab network\r\n", ErrNotSSH, "no SSH identification"},
		{"empty close", "", ErrUnreachable, "without sending data"},
		{"garbage", "\x00\x01\x02\x03garbage\r\n", ErrNotSSH, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadVersion(strings.NewReader(tc.input))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want it to match %v", err, tc.want)
			}
			if tc.msg != "" && !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("message %q should mention %q", err.Error(), tc.msg)
			}
		})
	}
}

// TestReadVersionRejectsObsoleteProtocol: the modern OpenSSH client refuses SSH-1,
// so reporting "ready" would set the user up for a failure with a confusing
// message from ssh instead of a clear one from vbox-tui.
func TestReadVersionRejectsObsoleteProtocol(t *testing.T) {
	for _, line := range []string{"SSH-1.99-OpenSSH_9.9", "SSH-2.0-OpenSSH_9.9"} {
		sv, err := ReadVersion(strings.NewReader(line + "\r\n"))
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		// 1.99 means "I speak both"; the client will negotiate 2.0.
		if sv.Protocol != "1.99" && sv.Protocol != "2.0" {
			t.Errorf("%s -> protocol %q", line, sv.Protocol)
		}
	}
	for _, line := range []string{"SSH-1.5-Cisco-1.25", "SSH-1.33-somebox"} {
		if _, err := ReadVersion(strings.NewReader(line + "\r\n")); !errors.Is(err, ErrNotSSH) {
			t.Errorf("%s should be refused as unusable, got %v", line, err)
		}
	}
	// A malformed identification must not be accepted as a success. What matters is
	// that none of these report "ready", because connecting would fail inside ssh
	// with a message the user cannot act on.
	for _, line := range []string{"SSH-\r\n", "SSH-2.0\r\n", "SSH-x y-implement\r\n"} {
		if sv, err := ReadVersion(strings.NewReader(line)); err == nil {
			t.Errorf("%q was accepted as a valid identification: %+v", line, sv)
		}
	}
	// Truncated input with no terminator at all is also not a readiness signal.
	if sv, err := ReadVersion(strings.NewReader("SSH-2.0-OpenSSH_9.9")); err == nil {
		t.Errorf("an unterminated identification was accepted: %+v", sv)
	}
}

// TestReadVersionIsBounded proves a hostile or broken peer cannot make vbox-tui
// allocate without limit. Two limits matter: total bytes and per-line length.
func TestReadVersionIsBounded(t *testing.T) {
	t.Run("huge single line", func(t *testing.T) {
		// One line well past the per-line cap, with no terminator.
		r := io.LimitReader(strings.NewReader(strings.Repeat("A", 64<<10)), 64<<10)
		done := make(chan error, 1)
		go func() {
			_, err := ReadVersion(r)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Error("a 64 KiB unterminated line must not be accepted")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ReadVersion did not return: the read is not bounded")
		}
	})

	t.Run("many banner lines", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 20000; i++ {
			fmt.Fprintf(&b, "banner line %d\r\n", i)
		}
		done := make(chan error, 1)
		go func() {
			// The reader has no end in sight from the parser's point of view:
			// it must give up on its own count or byte bound well before EOF.
			_, err := ReadVersion(strings.NewReader(b.String()))
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrNotSSH) {
				t.Errorf("want ErrNotSSH, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("an endless banner stream was not stopped")
		}
	})
}

// waitForSSHTestProber returns a prober tuned so tests finish quickly while still
// exercising the retry loop.
func waitForSSHTestProber(dial DialFunc, interval time.Duration) *Prober {
	return &Prober{
		Dial:           dial,
		Interval:       interval,
		AttemptTimeout: 2 * time.Second,
		NonSSHFailures: 2,
	}
}

// TestWaitForSSHSucceedsAgainstARealListener uses a loopback listener that speaks
// the banner, so the whole path (dial, deadline, read, parse) is real.
func TestWaitForSSHSucceedsAgainstARealListener(t *testing.T) {
	ln := startListener(t, func(c net.Conn) {
		_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6p1\r\n"))
	})
	p := waitForSSHTestProber(DefaultDial, 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sv, err := p.WaitForSSH(ctx, "127.0.0.1", portOf(t, ln))
	if err != nil {
		t.Fatalf("WaitForSSH: %v", err)
	}
	if sv.Implementation != "OpenSSH_9.6p1" {
		t.Errorf("version = %+v", sv)
	}
}

// TestWaitForSSHRetriesUntilReady models a booting guest: the first attempts are
// refused, then sshd comes up. A single failed connect must not end the wait,
// because that is exactly the situation start-and-connect exists for.
func TestWaitForSSHRetriesUntilReady(t *testing.T) {
	var attempts int32
	ln := startListener(t, func(c net.Conn) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			// Behave like a port nothing is listening on yet.
			_ = c.Close()
			return
		}
		_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
	})
	p := waitForSSHTestProber(DefaultDial, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sv, err := p.WaitForSSH(ctx, "127.0.0.1", portOf(t, ln))
	if err != nil {
		t.Fatalf("the wait should retry past a closed port: %v", err)
	}
	if sv.Implementation != "OpenSSH_9.6" {
		t.Errorf("version = %+v", sv)
	}
	if atomic.LoadInt32(&attempts) < 3 {
		t.Errorf("expected at least 3 attempts, got %d", attempts)
	}
}

// TestWaitForSSHReportsTimeout covers a port that accepts but never speaks. This
// is a real failure mode: a NAT rule pointing at a closed guest port, or a proxy
// that accepts eagerly.
func TestWaitForSSHReportsTimeout(t *testing.T) {
	var conns int32
	ln := startListener(t, func(c net.Conn) {
		atomic.AddInt32(&conns, 1)
		// Hold the connection open and say nothing.
		holdOpen(c)
	})
	p := waitForSSHTestProber(DefaultDial, 20*time.Millisecond)
	p.AttemptTimeout = 60 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := p.WaitForSSH(ctx, "127.0.0.1", portOf(t, ln))
	if err == nil {
		t.Fatal("a silent listener must not be reported as ready")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("want an error matching ErrTimeout, got %v (%T)", err, err)
	}
	if !strings.Contains(err.Error(), "attempt") {
		t.Errorf("the message should say how many tries were made: %q", err.Error())
	}
	// The deadline must actually bound the wait.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the wait overran its deadline: %s", elapsed)
	}
}

// TestWaitForSSHStopsOnANonSSHService: a wrong service is a permanent condition.
// Retrying it for the whole timeout would make a misconfigured NAT rule look like
// a slow boot.
func TestWaitForSSHStopsOnANonSSHService(t *testing.T) {
	var conns int32
	ln := startListener(t, func(c net.Conn) {
		atomic.AddInt32(&conns, 1)
		_, _ = c.Write([]byte("HTTP/1.0 200 OK\r\nServer: something-not-ssh\r\n\r\n"))
		_ = c.Close()
	})
	p := waitForSSHTestProber(DefaultDial, 10*time.Millisecond)
	p.NonSSHFailures = 2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := p.WaitForSSH(ctx, "127.0.0.1", portOf(t, ln))
	if !errors.Is(err, ErrNotSSH) {
		t.Fatalf("want ErrNotSSH, got %v", err)
	}
	if n := atomic.LoadInt32(&conns); n > 4 {
		t.Errorf("gave up too late: %d connections to a service that is plainly not SSH", n)
	}
	if !strings.Contains(err.Error(), "did not identify as an SSH server") {
		t.Errorf("the message should name the real problem: %q", err.Error())
	}
}

// TestWaitForSSHIsCancellable: Esc must return promptly. The message the UI shows
// then explains that cancelling does not recall a VirtualBox operation.
func TestWaitForSSHIsCancellable(t *testing.T) {
	ln := startListener(t, holdOpen)
	p := waitForSSHTestProber(DefaultDial, 30*time.Millisecond)
	p.AttemptTimeout = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := p.WaitForSSH(ctx, "127.0.0.1", portOf(t, ln))
		done <- err
	}()
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v (%T)", err, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling did not stop the wait")
	}
}

// TestWaitForSSHRefusedPortKeepsRetrying models a VM whose sshd has not started:
// the connect is refused, which is retryable, unlike a wrong service.
func TestWaitForSSHRefusedPortKeepsRetrying(t *testing.T) {
	// Bind and close so the port is very likely to refuse rather than hang.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback networking in this environment")
	}
	port := portOf(t, ln)
	_ = ln.Close()

	var attempts int32
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		atomic.AddInt32(&attempts, 1)
		return DefaultDial(ctx, addr)
	}
	p := waitForSSHTestProber(dial, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = p.WaitForSSH(ctx, "127.0.0.1", port)
	if err == nil {
		t.Fatal("a refused port must not be reported as ready")
	}
	if errors.Is(err, ErrNotSSH) {
		t.Errorf("a refused port is unreachable, not 'not SSH': %v", err)
	}
	if !errors.Is(err, ErrTimeout) && !errors.Is(err, ErrUnreachable) {
		t.Errorf("want ErrTimeout or ErrUnreachable, got %v", err)
	}
	if atomic.LoadInt32(&attempts) < 2 {
		t.Errorf("a retryable failure should have been retried, attempts = %d", attempts)
	}
}

// TestWaitForSSHReportsProgress lets the UI show why it is still waiting instead
// of appearing frozen behind a spinner.
func TestWaitForSSHReportsProgress(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	ln := startListener(t, func(c net.Conn) { _ = c.Close() })
	p := waitForSSHTestProber(DefaultDial, 15*time.Millisecond)
	p.Progress = func(s string) {
		mu.Lock()
		lines = append(lines, s)
		mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, _ = p.WaitForSSH(ctx, "127.0.0.1", portOf(t, ln))

	mu.Lock()
	defer mu.Unlock()
	if len(lines) == 0 {
		t.Fatal("no progress was reported while waiting")
	}
	for _, l := range lines {
		if !strings.Contains(l, "127.0.0.1") {
			t.Errorf("a progress line should name the endpoint: %q", l)
		}
	}
}

// TestWaitForSSHNilProberAndDefaults: a zero-value Prober must fall back to
// sensible values rather than divide by zero or spin.
func TestWaitForSSHNilProberAndDefaults(t *testing.T) {
	ln := startListener(t, func(c net.Conn) {
		_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.9\r\n"))
	})
	var p *Prober
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sv, err := p.WaitForSSH(ctx, "127.0.0.1", portOf(t, ln))
	if err != nil {
		t.Fatalf("nil prober: %v", err)
	}
	if sv.Protocol != "2.0" {
		t.Errorf("protocol = %q", sv.Protocol)
	}

	zero := &Prober{Dial: DefaultDial}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	// Interval is forced to the default (1s), so give it room for one retry.
	zero.Interval = 0
	if _, err := zero.WaitForSSH(ctx2, "127.0.0.1", portOf(t, ln)); err != nil {
		t.Errorf("zero-value prober: %v", err)
	}
}

func TestTimeoutErrorMatchesErrTimeout(t *testing.T) {
	// The last attempt's error is preserved as a wrapped error, so a caller can
	// still tell "nothing was listening" from "something answered wrongly" after
	// the overall wait expired.
	last := fmt.Errorf("%w: connection refused", ErrUnreachable)
	err := timeoutError("127.0.0.1:2222", 3, last)
	if !errors.Is(err, ErrTimeout) {
		t.Error("the returned error must match ErrTimeout so callers can branch on it")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("the last attempt's error should remain reachable: %v", err)
	}
	for _, want := range []string{"127.0.0.1:2222", "3 attempt", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message should mention %q: %q", want, err.Error())
		}
	}
}

// ---------- helpers ----------

// startListener runs a loopback TCP server for one test. Cleanup closes the
// listener *and* every accepted connection, so a handler that is deliberately
// holding a connection open (the silent-listener cases) returns instead of
// hanging the test binary. Handlers therefore never need a timeout of their own.
func startListener(t *testing.T, handler func(net.Conn)) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable: %v", err)
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				handler(c)
			}(c)
		}
	}()
	return ln
}

// holdOpen keeps a connection open and says nothing, which is what a NAT rule
// pointing at a not-yet-listening guest port looks like from the host when
// something else accepts the connection first. It returns as soon as either side
// closes, so it cannot outlive the test.
func holdOpen(c net.Conn) {
	_, _ = io.Copy(io.Discard, c)
}

func portOf(t *testing.T, ln net.Listener) int {
	t.Helper()
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}
	return tcpAddr.Port
}
