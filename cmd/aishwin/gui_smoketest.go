package main

import (
	"runtime"
	"syscall"
	"time"
)

func runGUISmokeTest() {
	runtime.LockOSThread()

	go func() {
		time.Sleep(500 * time.Millisecond)
		AppendLog("aishwin: smoke test window")
		AppendLog("line two")
		AppendLog("line three -- checking auto-scroll works")
		SetStatus("smoke test  |  session: none  |  v0.0.0-smoketest")
	}()

	buildMenu := func() syscall.Handle {
		bar := NewMenuBar()
		file := NewSubmenu(bar, "File")
		AddMenuItem(file, "Test", func() { AppendLog("File > Test clicked") })
		AddMenuSeparator(file)
		AddMenuItem(file, "Quit", func() { Quit() })
		return bar
	}

	err := StartGUI("aishwin smoke test", buildMenu, func() {
		AppendLog("window closing")
	})
	if err != nil {
		panic(err)
	}
}
