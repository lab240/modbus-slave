//go:build !windows

package main

func pauseIfOwnConsole() {}

func appMode(*opts) bool { return false }

func appRun(opts) {}
