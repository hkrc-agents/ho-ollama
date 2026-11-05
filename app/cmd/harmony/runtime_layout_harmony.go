//go:build harmonyos

package main

import "context"

type harmonyServerBackend interface {
	Start(context.Context, serverConfig) error
	Stop() error
}

type harmonyRuntimePaths struct {
	BinRoot    string
	Server     string
	Components map[string]string
}
