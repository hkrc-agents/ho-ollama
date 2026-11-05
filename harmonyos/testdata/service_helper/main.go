// Host-only lifecycle fixture, built WITHOUT -race even when its controller is
// race-tested: the race runtime reserves vastly more VA than the service limit.
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
)

func main() {
	port := ""
	for i, a := range os.Args {
		if a == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	if port == "" {
		os.Exit(9)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	listener, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(8)
	}
	if os.Getenv("PICO_HELPER_NO_BIND_EVENT") != "1" {
		fmt.Printf("srv          main: listening on http://127.0.0.1:%s\n", port)
	}
	if err := http.Serve(listener, mux); err != nil {
		os.Exit(8)
	}
}
