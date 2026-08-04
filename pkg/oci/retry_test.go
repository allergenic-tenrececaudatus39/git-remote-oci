package oci

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected time.Duration
	}{
		{"empty string", "", 0},
		{"integer seconds", "10", 10 * time.Second},
		{"zero seconds", "0", 0},
		{"invalid string", "invalid-seconds", 0},
		{"future HTTP date", time.Now().UTC().Add(30 * time.Second).Format(http.TimeFormat), 29 * time.Second}, // approximate
		{"past HTTP date", "Sun, 06 Nov 1994 08:49:37 GMT", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryAfter(tt.input)
			if tt.name == "future HTTP date" {
				if got < 20*time.Second || got > 35*time.Second {
					t.Errorf("parseRetryAfter(%q) = %v, expected approx 30s", tt.input, got)
				}
			} else if got != tt.expected {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestRetryTransport429WithRetryAfter(t *testing.T) {
	var attempts atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := attempts.Add(1)
		if count == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	rt := newRetryTransport(ts.Client().Transport)
	rt.initialInterval = 10 * time.Millisecond
	rt.maxInterval = 2 * time.Second

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	start := time.Now()
	resp, err := rt.RoundTrip(req)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200 OK, got %d", resp.StatusCode)
	}
	if attempts.Load() != 2 {
		t.Errorf("Expected 2 attempts, got %d", attempts.Load())
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("Expected at least 1s delay from Retry-After header, got %v", elapsed)
	}
}

func TestRetryTransportExponentialBackoff(t *testing.T) {
	var attempts atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := attempts.Add(1)
		if count <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	rt := newRetryTransport(ts.Client().Transport)
	rt.initialInterval = 20 * time.Millisecond
	rt.maxInterval = 100 * time.Millisecond

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200 OK, got %d", resp.StatusCode)
	}
	if attempts.Load() != 3 {
		t.Errorf("Expected 3 attempts, got %d", attempts.Load())
	}
}

func TestRetryTransportContextCancellation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	rt := newRetryTransport(ts.Client().Transport)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	_, err = rt.RoundTrip(req)
	if err == nil {
		t.Fatal("Expected error on cancelled context, got nil")
	}
}

func TestRetryTransportBodyRewind(t *testing.T) {
	var attempts atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload-data" {
			t.Errorf("Expected payload-data, got %q", string(body))
		}
		count := attempts.Add(1)
		if count == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	rt := newRetryTransport(ts.Client().Transport)
	rt.initialInterval = 10 * time.Millisecond

	bodyData := []byte("payload-data")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL, bytes.NewReader(bodyData))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("Expected status 201 Created, got %d", resp.StatusCode)
	}
	if attempts.Load() != 2 {
		t.Errorf("Expected 2 attempts, got %d", attempts.Load())
	}
}
