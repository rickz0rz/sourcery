package stream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestFFmpegArgsOrderAndContent(t *testing.T) {
	args := ffmpegArgs("https://x.test/live.m3u8", map[string]string{"Referer": "https://x.test/"})
	joined := strings.Join(args, " ")

	// -headers must come before -i so it applies to the input and its segments.
	hi := indexOf(args, "-headers")
	ii := indexOf(args, "-i")
	if hi < 0 || ii < 0 || hi > ii {
		t.Fatalf("-headers must precede -i: %v", args)
	}
	if args[ii+1] != "https://x.test/live.m3u8" {
		t.Errorf("input URL misplaced: %v", args)
	}
	for _, want := range []string{"-c copy", "-f mpegts", "pipe:1", "-nostdin"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
}

func TestFormatHeaders(t *testing.T) {
	got := formatHeaders(map[string]string{"Referer": "https://x.test/"})
	if got != "Referer: https://x.test/\r\n" {
		t.Errorf("formatHeaders = %q", got)
	}
	if formatHeaders(nil) != "" {
		t.Error("empty headers should format to an empty string")
	}
}

func TestFFmpegAvailable(t *testing.T) {
	if NewFFmpeg("definitely-not-a-real-binary-xyz").Available() {
		t.Error("a nonexistent binary reported available")
	}
}

// End-to-end through the real ffmpeg: remux an HLS playlist served over HTTP to
// MPEG-TS and confirm the bytes come out packet-aligned. Skipped where ffmpeg
// is absent.
func TestFFmpegRemuxesHLS(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	url := serveTestHLS(t)

	up, err := NewFFmpeg("ffmpeg").Open(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer up.Close()

	// Read the first chunk within a bounded time; a remux should produce output
	// promptly.
	type result struct {
		n   int
		err error
		buf []byte
	}
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, ReadSize)
		n, err := up.Read(buf)
		done <- result{n, err, buf}
	}()

	select {
	case r := <-done:
		if r.err != nil && r.err != io.EOF {
			t.Fatalf("read: %v", r.err)
		}
		if r.n == 0 {
			t.Fatal("ffmpeg produced no output")
		}
		if r.buf[0] != 0x47 {
			t.Errorf("output is not a transport stream (first byte %#x, want 0x47)", r.buf[0])
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ffmpeg produced no output in time")
	}
}

// Closing the ffmpeg upstream must stop the process and unblock a pending Read,
// the same contract a device connection has.
func TestFFmpegCloseUnblocksRead(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	url := serveTestHLS(t)

	up, err := NewFFmpeg("ffmpeg").Open(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, ReadSize)
		for {
			if _, err := up.Read(buf); err != nil {
				return
			}
		}
	}()

	up.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not unblock Read")
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// serveTestHLS generates a short HLS stream with ffmpeg and serves it over HTTP,
// returning the playlist URL. Using ffmpeg to build the fixture keeps the
// segments genuinely valid.
func serveTestHLS(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmd := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
		"-c:v", "mpeg2video",
		"-f", "hls", "-hls_time", "1", "-hls_list_size", "0",
		dir+"/index.m3u8")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test HLS: %v\n%s", err, out)
	}

	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return srv.URL + "/index.m3u8"
}
