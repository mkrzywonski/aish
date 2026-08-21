package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// console owns the single stdin-reading goroutine for this process and
// routes each typed line to whichever consumer is active: a pending
// askYN() call if one is waiting for an answer, otherwise the menu
// (menu.go). A single router is what makes a blocking line read safe to
// give a timeout in Go (a Read can't be interrupted directly, and two
// goroutines racing to read the same underlying stdin would corrupt input)
// while still letting the human use the same console for both approval
// prompts and menu commands without them stepping on each other. Only one
// prompt can be waiting at a time; a second concurrent one fails fast
// rather than racing the first for the next typed line.
type console struct {
	mu      sync.Mutex
	waiting chan string
}

var theConsole = &console{}

func init() {
	go theConsole.run()
}

func (c *console) run() {
	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)

		c.mu.Lock()
		waiting := c.waiting
		c.mu.Unlock()

		if waiting != nil {
			waiting <- line
			continue
		}
		if line != "" {
			handleMenuLine(line)
		}
	}
}

// askYN prints question to the console and blocks for the next line typed,
// treating it as the answer. Returns "" if it isn't y/n, if another prompt
// is already waiting, if stdin closed, or if the human doesn't respond
// within timeout. This is the human-facing half of the approval handshake
// aicmdd's duplicated connauth state machine drives
// (internal/aicmdd/auth.go) -- mirrors the spirit of
// internal/session/console.go's Prompt, but as a plain console prompt:
// there's no PTY here to divert keystrokes from, since this console is the
// human's own terminal, not a shared one.
func askYN(question string, timeout time.Duration) string {
	ch := make(chan string, 1)

	theConsole.mu.Lock()
	if theConsole.waiting != nil {
		theConsole.mu.Unlock()
		return ""
	}
	theConsole.waiting = ch
	theConsole.mu.Unlock()
	defer func() {
		theConsole.mu.Lock()
		theConsole.waiting = nil
		theConsole.mu.Unlock()
	}()

	fmt.Fprintf(stdout, "\n%s [y/n]: ", question)
	select {
	case line := <-ch:
		switch strings.ToLower(line) {
		case "y":
			return "y"
		case "n":
			return "n"
		default:
			fmt.Fprintln(stdout, "aicmd: unrecognized answer, treating as no response")
			return ""
		}
	case <-time.After(timeout):
		fmt.Fprintln(stdout, "\naicmd: prompt timed out")
		return ""
	}
}
