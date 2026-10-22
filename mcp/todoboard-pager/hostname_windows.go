//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// computerFQDN 取本机 FQDN：ComputerNameDnsFullyQualified，失败回退 hostname。
func computerFQDN() string {
	size := uint32(256)
	buf := make([]uint16, size)
	if err := windows.GetComputerNameEx(windows.ComputerNameDnsFullyQualified, &buf[0], &size); err == nil && size > 0 {
		return windows.UTF16ToString(buf[:size])
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}
