//go:build !windows

package main

import "os"

func computerFQDN() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}
