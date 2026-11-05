//go:build harmonyos

// The HarmonyOS host replaces only the native desktop window/launcher.
// HTTP routing, chat, SQLite storage and React assets are upstream packages.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/ui"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--model-store-worker" {
		if err := harmonyStoreWorker(os.Args[2:], os.Stdout); err != nil {
			json.NewEncoder(os.Stdout).Encode(map[string]string{"error": err.Error()})
			slog.Error("model store worker", "error", err)
			os.Exit(1)
		}
		return
	}
	address := flag.String("listen", "127.0.0.1:11435", "APP HTTP listen address (loopback only)")
	data := flag.String("data", "", "application-private APP data directory")
	flag.Parse()
	host, _, err := net.SplitHostPort(*address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || *data == "" {
		slog.Error("explicit private data directory and loopback listen address required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*data, 0700); err != nil {
		slog.Error("mkdir", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".ollama", "logs"), 0700); err != nil {
		panic(err)
	}
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	layout, err := harmonyRuntimeLayout(exe)
	if err != nil {
		panic(err)
	}
	shutdownIdentity, err := harmonySelfShutdownIdentity(exe)
	if err != nil {
		panic(err)
	}
	// Native spawn also removes this file. Never reuse an earlier instance's ACK.
	if err := os.Remove(filepath.Join(*data, "shutdown-complete")); err != nil && !os.IsNotExist(err) {
		panic(err)
	}
	os.Setenv("PATH", harmonyCommandPath(layout.BinRoot, os.Getenv("PATH")))
	// Read in the actual HNP namespace, not HDC's different debug mount. This
	// makes stale installed binaries distinguishable from newly packaged files.
	identities := map[string]string{}
	for name, path := range layout.Components {
		file, err := os.Open(path)
		if err != nil {
			panic(err)
		}
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		file.Close()
		if err != nil {
			panic(err)
		}
		identities[name] = hex.EncodeToString(hash.Sum(nil))
	}
	identity, err := json.Marshal(identities)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(*data, "runtime-sha256.json"), identity, 0600); err != nil {
		panic(err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(secret)
	// Host reads this private file to install the WebView's HttpOnly cookie.
	if err := os.WriteFile(filepath.Join(*data, "app-token"), []byte(token), 0600); err != nil {
		panic(err)
	}
	// Same private database/onboarding location as the CLI's read-only lookup.
	database := &store.Store{}
	defer database.Close()
	restart := make(chan struct{}, 1)
	s := &ui.Server{Store: database, Token: token, Logger: slog.Default(), Restart: func() {
		select {
		case restart <- struct{}{}:
		default:
		}
	}}
	signalContext, signalStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer signalStop()
	ctx, stop := harmonyAppContext(signalContext)
	defer stop()
	coreService := newHarmonyCore(database)
	standalone := newHarmonyServer(*data, layout.Server)
	standalone.lifetime = ctx
	gguf := newHarmonyGGUF(*data)
	storage := newHarmonyStorageManager(*data, token, coreService, standalone, ctx)
	gguf.inspect = storage.verifiedModel
	server := &http.Server{Addr: *address, Handler: storage.wrap(withStorageProbe(withHarmonyServices(withModelManagementStore(s.Handler(), token, *data, gguf), token, standalone, coreService, ctx, storage.inventory), token, *data, ctx)), ReadHeaderTimeout: 10 * time.Second}
	// Cancelling the host also cancels active request-scoped native workers.
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		slog.Error("APP listen", "error", err)
		os.Exit(1)
	}
	defer listener.Close()
	go func() {
		if err := gguf.recoverStaging(); err != nil {
			slog.Error("GGUF staging recovery", "error", err)
		}
		if err := storage.Recover(); err != nil {
			slog.Error("storage recovery", "error", err)
			return
		}
		storage.gate.RLock()
		harmonyServiceActions.Lock()
		if wanted, err := harmonyCoreShouldStart(); err != nil {
			slog.Error("core desired-state recovery", "error", err)
		} else if !wanted {
			slog.Info("core remains stopped by current-session user intent")
		} else if err := coreService.Start(ctx); err != nil {
			slog.Error("core start", "error", err)
		} else {
			go func() {
				storage.gate.RLock()
				defer storage.gate.RUnlock()
				check, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				if err := gguf.reconcilePending(check); err != nil {
					slog.Warn("import recovery", "error", err)
				}
			}()
		}
		if err := harmonyRestoreStandalone(ctx, standalone); err != nil {
			slog.Error("standalone desired-state recovery", "error", err)
		}
		harmonyServiceActions.Unlock()
		storage.gate.RUnlock()
		for {
			select {
			case <-ctx.Done():
				return
			case <-restart:
				storage.gate.RLock()
				harmonyServiceActions.Lock()
				if wanted, err := harmonyCoreShouldStart(); err != nil {
					slog.Error("core restart intent", "error", err)
				} else if wanted {
					if err := coreService.Restart(ctx); err != nil {
						slog.Error("core restart", "error", err)
					}
				}
				harmonyServiceActions.Unlock()
				storage.gate.RUnlock()
			}
		}
	}()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		for {
			err := finishHarmonyHTTPShutdown(server, 5*time.Second, func() error {
				return finishHarmonyStorageShutdown(&storage.gate, func() error {
					return completeHarmonyShutdown(standalone.stop, coreService.Stop, func() error {
						return harmonyWriteShutdownReceipt(*data, shutdownIdentity)
					})
				})
			})
			if err == nil {
				break
			}
			// Stay alive and retain ownership records on unknown/live cleanup.
			// HAP must not remove the tray or terminate while this is unresolved.
			slog.Error("APP shutdown incomplete; retaining host", "error", err)
			time.Sleep(time.Second)
		}
	}()
	slog.Info("official Ollama APP service", "listen", *address)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		slog.Error("APP HTTP", "error", err)
		stop()
	}
	<-shutdownDone
}
