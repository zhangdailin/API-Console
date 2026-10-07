package handler

import (
	"syscall"
	"unsafe"
)

var relayCounter = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
var relayCounterFrequency = func() int64 {
	var frequency int64
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
	ok, _, _ := proc.Call(uintptr(unsafe.Pointer(&frequency)))
	if ok == 0 || frequency <= 0 {
		panic("performance counter unavailable")
	}
	return frequency
}()

// The wall clock on some Windows hosts rounds sub-millisecond samples to zero.
func relayBenchNow() int64 {
	var counter int64
	ok, _, _ := relayCounter.Call(uintptr(unsafe.Pointer(&counter)))
	if ok == 0 {
		panic("performance counter unavailable")
	}
	return counter
}

func relayBenchElapsed(start int64) int64 {
	return (relayBenchNow() - start) * 1000000000 / relayCounterFrequency
}
