//go:build harmonyos

package main

import (
	"crypto/subtle"
	"net/http"
	"net/http/httputil"

	"github.com/ollama/ollama/envconfig"
)

// Expose only the existing core operations needed by the HarmonyOS model page.
// No quantization/model lifecycle logic is duplicated here. APP auth remains mandatory.
func withModelManagement(app http.Handler, token string) http.Handler {
	return modelManagementHandler(app, token, nil, nil)
}

// directory must be the APP's private data directory. Initialization failures
// fail closed on download endpoints; the legacy entry point remains stateless.
func withModelManagementStore(app http.Handler, token, directory string, supplied ...*harmonyGGUF) http.Handler {
	gguf := newHarmonyGGUF(directory)
	if len(supplied) > 0 {
		gguf = supplied[0]
	}
	return modelManagementHandler(app, token, openHarmonyDownloads(directory), gguf)
}

func modelManagementHandler(app http.Handler, token string, downloads *harmonyDownloads, gguf *harmonyGGUF) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(envconfig.ConnectableHost())
	direct := proxy.Director
	proxy.Director = func(request *http.Request) {
		direct(request)
		request.Header.Del("Cookie")
	}
	protect := func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie("token")
			if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 {
				http.Error(w, "Token is required", http.StatusForbidden)
				return
			}
			handler.ServeHTTP(w, r)
		})
	}
	protected := protect(proxy)
	mux := http.NewServeMux()
	mux.Handle("GET /api/ps", protected)
	mux.Handle("POST /api/generate", protected)
	mux.Handle("DELETE /api/delete", protected)
	if gguf != nil {
		mux.Handle("POST /api/harmony/gguf", protect(http.HandlerFunc(gguf.importFile)))
		mux.Handle("GET /api/harmony/imports", protect(http.HandlerFunc(gguf.list)))
		mux.Handle("POST /api/harmony/imports/reconcile", protect(http.HandlerFunc(gguf.reconcile)))
		mux.Handle("POST /api/harmony/imports/archive", protect(http.HandlerFunc(gguf.archive)))
	}
	if downloads == nil {
		mux.Handle("POST /api/pull", protected)
	} else {
		mux.Handle("POST /api/pull", protect(http.HandlerFunc(downloads.pull)))
		mux.Handle("GET /api/harmony/downloads", protect(http.HandlerFunc(downloads.list)))
		mux.Handle("DELETE /api/harmony/downloads", protect(http.HandlerFunc(downloads.forget)))
	}
	mux.Handle("/", app)
	return mux
}
