package stream

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestCopyToRelaysEverything(t *testing.T) {
	// Larger than the copy buffer, so multiple iterations are exercised.
	payload := bytes.Repeat([]byte{0x47, 0xAA, 0xBB, 0xCC}, bufferSize)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	up, err := NewProxy().Open(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer up.Close()

	rec := httptest.NewRecorder()
	n, err := up.CopyTo(rec)
	if err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("relayed %d bytes, want %d", n, len(payload))
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Error("relayed bytes differ from what the device sent")
	}
}

// Cancelling the request must end the relay, because that is what closes the
// upstream connection and makes the device release its tuner.
func TestCancellationEndsTheRelay(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("start"))
		http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	up, err := NewProxy().Open(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer up.Close()

	done := make(chan error, 1)
	go func() {
		_, err := up.CopyTo(httptest.NewRecorder())
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		// This is how nearly every stream ends. Reporting it as a failure
		// would bury the real errors among the routine ones.
		if err != nil {
			t.Errorf("CopyTo returned %v, want nil for a normal disconnect", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not stop after the request was cancelled")
	}
}

// The buffer must hold whole transport stream packets.
func TestBufferIsPacketAligned(t *testing.T) {
	const packet = 188
	if bufferSize%packet != 0 {
		t.Errorf("bufferSize %d is not a multiple of %d", bufferSize, packet)
	}
}
