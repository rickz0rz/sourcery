package stream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A device with no free tuner answers with an error status rather than
// blocking. That is how the caller learns to try another candidate, so it must
// surface as an error and not as an empty stream.
func TestOpenRejectsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no free tuner", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := NewProxy().Open(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for a 503 response")
	}
}

func TestOpenFailsOnUnreachableDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // refuse connections

	if _, err := NewProxy().Open(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for a refused connection")
	}
}

func TestReadDeliversTheStream(t *testing.T) {
	// Larger than one read, so multiple reads are exercised.
	payload := bytes.Repeat([]byte{0x47, 0xAA, 0xBB, 0xCC}, ReadSize)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	up, err := NewProxy().Open(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer up.Close()

	got, err := io.ReadAll(readerOf(up))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("read %d bytes, want the %d sent", len(got), len(payload))
	}
}

// Closing the upstream unblocks a Read in progress, which is how the fan-out
// reader is stopped once nobody is watching.
func TestCloseUnblocksRead(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("start"))
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	up, err := NewProxy().Open(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Drain the first chunk so the next Read blocks waiting for more.
	buf := make([]byte, ReadSize)
	if _, err := up.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := up.Read(buf); err != nil {
				return
			}
		}
	}()

	up.Close()
	<-done // must return; if Close did not unblock Read this hangs and the test times out
}

func TestReadSizeIsPacketAligned(t *testing.T) {
	const packet = 188
	if ReadSize%packet != 0 {
		t.Errorf("ReadSize %d is not a multiple of %d", ReadSize, packet)
	}
}

// readerOf adapts an Upstream to io.Reader for the convenience of io.ReadAll.
func readerOf(u *Upstream) io.Reader { return readerFunc(u.Read) }

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
