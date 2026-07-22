package stream

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// FFmpeg opens a stream by delegating to the ffmpeg binary, which reads the
// source container -- notably an HLS playlist -- and remuxes it to a raw MPEG-TS
// byte stream that the rest of Sourcery relays like any other.
//
// ffmpeg is used because HLS is deceptively involved (live playlist refresh,
// segment encryption, discontinuities), and it already does all of it while
// passing the configured headers through to segment requests. Remuxing with
// -c copy performs no transcoding, so the cost is small.
type FFmpeg struct {
	path string
}

// NewFFmpeg returns an FFmpeg opener using the given binary, or "ffmpeg" from
// PATH when path is empty.
func NewFFmpeg(path string) *FFmpeg {
	if path == "" {
		path = "ffmpeg"
	}
	return &FFmpeg{path: path}
}

// Available reports whether the ffmpeg binary can be found.
func (f *FFmpeg) Available() bool {
	_, err := exec.LookPath(f.path)
	return err == nil
}

// Open starts ffmpeg and returns its output as an Upstream. The process runs
// until the Upstream is closed, which kills it and releases whatever it held.
//
// ctx bounds only the startup; the stream itself lives until Close, because it
// outlives the request that opened it, exactly like a device connection.
func (f *FFmpeg) Open(ctx context.Context, url string, headers map[string]string) (*Upstream, error) {
	cmd := exec.Command(f.path, ffmpegArgs(url, headers)...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}
	errBuf := &ringBuffer{max: 4096}
	cmd.Stderr = errBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg (%s): %w", f.path, err)
	}
	return &Upstream{body: &ffmpegBody{cmd: cmd, stdout: stdout, errBuf: errBuf}}, nil
}

// ffmpegArgs builds the command line. -headers must precede -i so it applies to
// the input, and ffmpeg carries those headers to the HLS segment requests too.
func ffmpegArgs(url string, headers map[string]string) []string {
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		// Recover from transient drops rather than ending the stream.
		"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "2",
	}
	if h := formatHeaders(headers); h != "" {
		args = append(args, "-headers", h)
	}
	// -c copy remuxes without transcoding; +genpts keeps timestamps sane across
	// HLS segment boundaries.
	args = append(args, "-i", url, "-c", "copy", "-fflags", "+genpts", "-f", "mpegts", "pipe:1")
	return args
}

// formatHeaders renders headers in the CRLF-delimited form ffmpeg's -headers
// option expects.
func formatHeaders(headers map[string]string) string {
	var b strings.Builder
	for k, v := range headers {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\r\n")
	}
	return b.String()
}

// ffmpegBody adapts a running ffmpeg process to an io.ReadCloser: reads pull
// from its stdout, and Close kills it.
type ffmpegBody struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	errBuf *ringBuffer

	waitOnce  sync.Once
	waitErr   error
	closeOnce sync.Once
}

func (b *ffmpegBody) Read(p []byte) (int, error) {
	n, err := b.stdout.Read(p)
	if err == io.EOF {
		// ffmpeg ended. If it failed -- a bad URL, a required header missing,
		// an unsupported codec -- surface its stderr so the cause is visible
		// rather than the stream just appearing to end early.
		if werr := b.wait(); werr != nil {
			return n, fmt.Errorf("ffmpeg exited: %v: %s", werr, strings.TrimSpace(b.errBuf.String()))
		}
	}
	return n, err
}

func (b *ffmpegBody) Close() error {
	b.closeOnce.Do(func() {
		if b.cmd.Process != nil {
			b.cmd.Process.Kill()
		}
		b.stdout.Close()
		b.wait()
	})
	return nil
}

// wait reaps the process exactly once; both Read (on EOF) and Close may call it.
func (b *ffmpegBody) wait() error {
	b.waitOnce.Do(func() { b.waitErr = b.cmd.Wait() })
	return b.waitErr
}

// ringBuffer keeps the last max bytes written to it, for capturing the tail of
// ffmpeg's stderr without letting a chatty process grow unbounded.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
