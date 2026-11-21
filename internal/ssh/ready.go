package ssh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Readiness limits. A TCP connection alone does not prove the endpoint serves
// SSH: while a guest boots, the port may be closed, may be forwarded to
// something else, or sshd may still be starting.
const (
	// DefaultInterval is how long to wait between attempts.
	DefaultInterval = 1 * time.Second
	// DefaultAttemptTimeout bounds a single connect+banner exchange.
	DefaultAttemptTimeout = 4 * time.Second
	// maxBannerBytes bounds the exchange. RFC 4253 allows a server to send
	// arbitrary pre-authentication banner lines below the identification line,
	// capped at 8192 bytes in total; the limit here is deliberately larger so a
	// verbose guest is still understood, and it is what makes the read bounded.
	maxBannerBytes = 64 << 10
	// maxBannerLineBytes is the longest single banner line accepted.
	maxBannerLineBytes = 8 << 10
	// DefaultNonSSHLimit is how many consecutive non-SSH answers are tolerated
	// before giving up. A wrong service on the port is a permanent condition, so
	// retrying for the whole timeout would only waste the user's time.
	DefaultNonSSHLimit = 3
)

// Errors returned by the readiness check. They are distinguished because the UI
// must say something different for "nothing listening yet" than for "something
// else is listening".
var (
	// ErrNotSSH means a peer answered but did not present an SSH identification.
	ErrNotSSH = errors.New("endpoint is not speaking SSH")
	// ErrTimeout means the endpoint never became ready within the deadline.
	ErrTimeout = errors.New("timed out waiting for SSH")
	// ErrUnreachable covers connection refused, reset and name resolution
	// failures. It is retryable while the guest boots.
	ErrUnreachable = errors.New("endpoint is not accepting connections")
)

// ServerVersion is what the peer announced.
type ServerVersion struct {
	// Raw is the full identification line, e.g. SSH-2.0-OpenSSH_9.6p1.
	Raw string
	// Protocol is the "2.0" part.
	Protocol string
	// Implementation is the remainder, e.g. OpenSSH_9.6p1.
	Implementation string
	// BannerLines holds any pre-identification banner lines the server sent.
	BannerLines []string
}

// DialFunc dials a TCP address. Tests substitute a loopback listener.
type DialFunc func(ctx context.Context, addr string) (net.Conn, error)

// Prober waits for an endpoint to identify itself as an SSH server.
type Prober struct {
	Dial           DialFunc
	Interval       time.Duration
	AttemptTimeout time.Duration
	NonSSHFailures int
	// Progress, when set, receives one line per attempt for the UI to show.
	Progress func(string)
}

// NewProber returns a prober with production defaults.
func NewProber() *Prober {
	return &Prober{
		Dial:           DefaultDial,
		Interval:       DefaultInterval,
		AttemptTimeout: DefaultAttemptTimeout,
		NonSSHFailures: DefaultNonSSHLimit,
	}
}

// DefaultDial uses the standard library dialer honoured by ctx.
func DefaultDial(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: DefaultAttemptTimeout}
	return d.DialContext(ctx, "tcp", addr)
}

// WaitForSSH polls the endpoint until it sends an SSH identification line, ctx
// is cancelled, or the deadline in ctx passes. Cancelling stops the waiting
// only: it does not and cannot undo an already-issued VirtualBox operation.
func (p *Prober) WaitForSSH(ctx context.Context, host string, port int) (ServerVersion, error) {
	if p == nil {
		p = NewProber()
	}
	dial := p.Dial
	if dial == nil {
		dial = DefaultDial
	}
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	attempt := p.AttemptTimeout
	if attempt <= 0 {
		attempt = DefaultAttemptTimeout
	}
	nonSSHLimit := p.NonSSHFailures
	if nonSSHLimit <= 0 {
		nonSSHLimit = DefaultNonSSHLimit
	}
	addr := JoinHostPort(host, port)

	var (
		lastErr      error
		nonSSHStreak int
		attempts     int
	)
	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return ServerVersion{}, timeoutError(addr, attempts, lastErr)
			}
			return ServerVersion{}, err
		}
		attempts++
		sv, err := p.attempt(ctx, dial, addr, attempt)
		switch {
		case err == nil:
			return sv, nil
		case errors.Is(err, ErrNotSSH):
			nonSSHStreak++
			lastErr = err
			if nonSSHStreak >= nonSSHLimit {
				return ServerVersion{}, fmt.Errorf("%w: %s answered but did not identify as an SSH server%s",
					ErrNotSSH, addr, lastDetail(sv))
			}
		default:
			nonSSHStreak = 0
			lastErr = err
		}
		if p.Progress != nil {
			p.Progress(fmt.Sprintf("waiting for SSH on %s: %s", addr, lastErr))
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ServerVersion{}, timeoutError(addr, attempts, lastErr)
			}
			return ServerVersion{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (p *Prober) attempt(ctx context.Context, dial DialFunc, addr string, timeout time.Duration) (ServerVersion, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := dial(attemptCtx, addr)
	if err != nil {
		if ctx.Err() != nil {
			return ServerVersion{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return ServerVersion{}, fmt.Errorf("%w: connection attempt to %s timed out", ErrUnreachable, addr)
		}
		return ServerVersion{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer conn.Close()
	// A deadline on the connection bounds the banner read even if the caller's
	// dialer ignored ctx, so a silent listener cannot stall a whole attempt.
	if dl, ok := attemptCtx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	// A blocked socket read cannot observe a cancelled context, and a deadline
	// alone could make Esc wait out the whole attempt timeout. Closing the
	// connection is what actually interrupts the read, so a watchdog does that
	// when the wait is cancelled or its deadline arrives.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-attemptCtx.Done():
			_ = conn.Close()
		case <-stopWatch:
		}
	}()

	sv, err := ReadVersion(conn)
	if err == nil {
		return sv, nil
	}
	// Attribute an interrupted read to the reason it was interrupted, so a cancel
	// is reported as a cancel and never as "the endpoint is not SSH".
	switch {
	case ctx.Err() != nil:
		return ServerVersion{}, ctx.Err()
	case errors.Is(attemptCtx.Err(), context.DeadlineExceeded):
		return ServerVersion{}, fmt.Errorf("%w: timed out while reading the SSH identification from %s", ErrUnreachable, addr)
	}
	return sv, err
}

func lastDetail(sv ServerVersion) string {
	if sv.Raw != "" {
		return " (first line: " + sv.Raw + ")"
	}
	return ""
}

func timeoutError(addr string, attempts int, last error) error {
	msg := fmt.Sprintf("%s: %s after %d attempt(s)", ErrTimeout, addr, attempts)
	if last != nil {
		msg += "; last error: " + last.Error()
	}
	return &timeoutDetail{err: errors.New(msg), last: last}
}

// timeoutDetail keeps ErrTimeout matchable with errors.Is while preserving the
// last attempt's error.
type timeoutDetail struct {
	err  error
	last error
}

func (t *timeoutDetail) Error() string { return t.err.Error() }

func (t *timeoutDetail) Is(target error) bool {
	return target == ErrTimeout || errors.Is(t.err, target)
}

func (t *timeoutDetail) Unwrap() []error {
	if t.last == nil {
		return []error{t.err}
	}
	return []error{t.err, t.last}
}

// ReadVersion performs the identification exchange only. It reads the peer's
// banner lines and its SSH identification line, then returns. It never sends
// anything, so nothing is authenticated and no key material is exchanged.
//
// Reads are bounded by maxBannerBytes and maxBannerLineBytes, and by whatever
// deadline the caller put on the connection.
func ReadVersion(r io.Reader) (ServerVersion, error) {
	// The buffer is exactly one banner line wide, so ReadSlice hands back data in
	// chunks no larger than that: a peer that streams an unterminated line can
	// make this function allocate at most one line's worth before the limit cuts
	// it off.
	br := bufio.NewReaderSize(r, maxBannerLineBytes)
	var (
		sv     ServerVersion
		total  int
		banner []string
	)
	for {
		line, err := readLimitedLine(br, maxBannerLineBytes, &total)
		if err != nil {
			if errors.Is(err, errUnterminated) {
				// The peer cut the stream mid-line. A truncated identification is
				// not readiness: the OpenSSH client requires a CRLF-terminated
				// identification string, so reporting "ready" here would fail
				// inside ssh a moment later.
				if isIdentification(line) {
					return sv, fmt.Errorf("%w: the SSH identification was not terminated by a newline (%q)", ErrNotSSH, line)
				}
				if len(banner) > 0 || line != "" {
					if line != "" {
						banner = append(banner, line)
					}
					return sv, fmt.Errorf("%w: server sent a banner but no SSH identification%s", ErrNotSSH, joinDetail(banner))
				}
			}
			if len(banner) > 0 && errors.Is(err, io.EOF) {
				return sv, fmt.Errorf("%w: server sent a banner but no SSH identification%s", ErrNotSSH, joinDetail(banner))
			}
			if errors.Is(err, errLineTooLong) {
				return sv, fmt.Errorf("%w: %v", ErrNotSSH, err)
			}
			if errors.Is(err, io.EOF) {
				if total == 0 {
					return sv, fmt.Errorf("%w: the endpoint closed the connection without sending data", ErrUnreachable)
				}
				return sv, fmt.Errorf("%w: the endpoint sent %d byte(s) that were not an SSH identification%s", ErrNotSSH, total, joinDetail(banner))
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return sv, fmt.Errorf("%w: timed out while reading the SSH identification", ErrUnreachable)
			}
			return sv, err
		}
		if isIdentification(line) {
			sv.Raw = line
			sv.BannerLines = banner
			proto, impl, ok := parseIdentification(line)
			if !ok {
				return sv, fmt.Errorf("%w: malformed identification line %q", ErrNotSSH, line)
			}
			sv.Protocol = proto
			sv.Implementation = impl
			switch proto {
			case "2.0", "1.99":
				return sv, nil
			case "1.5", "1.33":
				return sv, fmt.Errorf("%w: %s offers only SSH protocol %s, which the OpenSSH client will refuse", ErrNotSSH, impl, proto)
			default:
				return sv, fmt.Errorf("%w: unsupported SSH protocol version %q", ErrNotSSH, proto)
			}
		}
		// A line that does not start with "SSH-" is a pre-banner message, legal
		// per RFC 4253. Keep counting so the overall exchange stays bounded.
		banner = append(banner, line)
		if total >= maxBannerBytes {
			return sv, fmt.Errorf("%w: banner exceeded %d bytes before the identification line", ErrNotSSH, maxBannerBytes)
		}
		if len(banner) > 64 {
			return sv, fmt.Errorf("%w: too many banner lines before the identification", ErrNotSSH)
		}
	}
}

var errLineTooLong = errors.New("banner line too long")

// errUnterminated marks a final line the peer ended without a line terminator.
// RFC 4253 requires the identification string to end with CRLF, and the OpenSSH
// client enforces that, so a truncated identification is not readiness.
var errUnterminated = errors.New("stream ended without a line terminator")

// readLimitedLine reads one line, never more than limit bytes, and reports how
// many bytes were consumed overall.
//
// It uses ReadSlice rather than ReadLine for two reasons: ReadLine would strip
// the terminator and hide a truncated final line, and its chunked ReadString
// shape complicates the limit check. ReadSlice hands back raw buffer chunks as
// they fill, which keeps memory bounded against a peer that streams a line with
// no terminator at all.
func readLimitedLine(br *bufio.Reader, limit int, total *int) (string, error) {
	var accum []byte
	for {
		chunk, err := br.ReadSlice('\n')
		full := errors.Is(err, bufio.ErrBufferFull)
		if len(chunk) > 0 {
			if len(accum)+len(chunk) > limit {
				return "", fmt.Errorf("%w: %d bytes in one line (limit %d)", errLineTooLong, len(accum)+len(chunk), limit)
			}
			accum = append(accum, chunk...)
			*total += len(chunk)
			if *total > maxBannerBytes {
				return "", fmt.Errorf("%w: banner exceeded %d bytes before the identification line", ErrNotSSH, maxBannerBytes)
			}
		}
		if full {
			continue // buffer exhausted; the line continues in the next chunk
		}
		if err != nil {
			if len(accum) > 0 {
				if errors.Is(err, io.EOF) {
					return trimEOL(string(accum)), errUnterminated
				}
				return trimEOL(string(accum)), err
			}
			return "", err
		}
		// Delimiter found: the line is complete and terminated.
		return trimEOL(string(accum)), nil
	}
}

func trimEOL(s string) string { return strings.TrimRight(s, "\r\n") }

func isIdentification(line string) bool { return strings.HasPrefix(line, "SSH-") }

// parseIdentification splits SSH-<protoversion>-<implementation>.
func parseIdentification(line string) (proto, impl string, ok bool) {
	rest := strings.TrimPrefix(line, "SSH-")
	idx := strings.IndexByte(rest, '-')
	if idx <= 0 {
		return "", "", false
	}
	return rest[:idx], rest[idx+1:], true
}

func joinDetail(banner []string) string {
	if len(banner) == 0 {
		return ""
	}
	first := banner[0]
	if len(first) > 60 {
		first = first[:60] + "…"
	}
	return fmt.Sprintf(" (first banner line: %q)", first)
}
