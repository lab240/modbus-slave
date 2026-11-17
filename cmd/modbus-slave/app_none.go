//go:build !windows && !linux

package main

func appMode(*opts) bool { return false }

func appRun(opts) {}
