//go:build !linux

package stats

import "errors"

// This tool provisions and runs on Linux; this file exists only so the
// package (and everything built on it, including tests) compiles on a
// development machine running something else. Nothing here is meant to work
// in production — see proc_linux.go for the real implementation.
var errUnsupported = errors.New("process sampling is only implemented on linux")

func PoolPIDs(slug string) ([]int, error)        { return nil, errUnsupported }
func ReadProcSample(pid int) (ProcSample, error) { return ProcSample{}, errUnsupported }
func clockTicksPerSecond() int                   { return 100 }

const procSupported = false
