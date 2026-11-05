//go:build harmonyos

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestModelManagementAuthAndForwarding(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Cookie") != "" {
			t.Error("APP cookie leaked to core")
		}
		if r.URL.Path == "/api/delete" {
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"model":"owned-test"}` {
				t.Errorf("changed request: %s", body)
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"models":[]}`))
	}))
	defer upstream.Close()
	t.Setenv("OLLAMA_HOST", upstream.URL)
	handler := withModelManagement(http.NotFoundHandler(), "private-secret")
	for _, cookie := range []string{"", "token=wrong"} {
		r := httptest.NewRequest("GET", "/api/ps", nil)
		r.Header.Set("Cookie", cookie)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("unauthorized status %d", w.Code)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("unauthenticated call reached core")
	}
	r := httptest.NewRequest("DELETE", "/api/delete", strings.NewReader(`{"model":"owned-test"}`))
	r.Header.Set("Cookie", "token=private-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || requests.Load() != 1 {
		t.Fatalf("status=%d requests=%d", w.Code, requests.Load())
	}
	r = httptest.NewRequest("GET", "/api/arbitrary", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound || requests.Load() != 1 {
		t.Fatal("proxy exposed arbitrary route")
	}
}

func TestModelPullAuthAndStreaming(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/pull" || r.Method != "POST" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected proxy request")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"test:tag","stream":true}` {
			t.Errorf("changed body: %s", body)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, "{\"status\":\"pulling\"}\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "{\"status\":\"success\"}\n")
	}))
	defer upstream.Close()
	t.Setenv("OLLAMA_HOST", upstream.URL)
	handler := withModelManagement(http.NotFoundHandler(), "private-secret")
	for _, cookie := range []string{"", "token=wrong", "token=private-secret"} {
		r := httptest.NewRequest("POST", "/api/pull", strings.NewReader(`{"model":"test:tag","stream":true}`))
		r.Header.Set("Cookie", cookie)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if cookie != "token=private-secret" {
			if w.Code != 403 {
				t.Fatal(w.Code)
			}
		} else if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"success"`) || !w.Flushed {
			t.Fatalf("stream lost: %d %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unauthenticated pull reached core")
	}
}
