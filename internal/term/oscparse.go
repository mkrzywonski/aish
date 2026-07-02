package term

import (
	"strconv"
	"strings"
	"sync"
)

// EventKind identifies a parsed control event in the output stream.
type EventKind int

const (
	EvPrompt   EventKind = iota // OSC 133;A — shell is at a prompt
	EvInput                     // OSC 133;B
	EvPreexec                   // OSC 133;C — command execution starting
	EvDone                      // OSC 133;D;<exit> — command finished
	EvCwd                       // OSC 7;file://host/path
	EvSentinel                  // OSC 7979;<nonce>;<exit> — aish in-band sentinel
)

// Event is one OSC occurrence, positioned by absolute ring offsets so
// consumers can slice the scrollback around it.
type Event struct {
	Kind   EventKind
	Exit   int    // EvDone, EvSentinel
	Nonce  string // EvSentinel
	Host   string // EvCwd
	Cwd    string // EvCwd
	Start  int64  // absolute offset of the ESC byte
	End    int64  // absolute offset just past the terminator
}

// OSCParser incrementally scans the output stream for OSC sequences,
// tolerating sequences split across writes. Feed must be called with
// monotonically increasing base offsets (the ring offset of p[0]).
type OSCParser struct {
	mu      sync.Mutex
	inOSC   bool
	sawEsc  bool  // last byte was a bare ESC (possible split before ']')
	payload []byte
	start   int64

	subsMu sync.Mutex
	subs   []chan Event
}

const maxOSCPayload = 4096

// Subscribe returns a channel receiving all future events. The channel is
// buffered; events are dropped for slow consumers rather than blocking the
// output pump.
func (p *OSCParser) Subscribe() chan Event {
	ch := make(chan Event, 64)
	p.subsMu.Lock()
	p.subs = append(p.subs, ch)
	p.subsMu.Unlock()
	return ch
}

func (p *OSCParser) Unsubscribe(ch chan Event) {
	p.subsMu.Lock()
	defer p.subsMu.Unlock()
	for i, c := range p.subs {
		if c == ch {
			p.subs = append(p.subs[:i], p.subs[i+1:]...)
			return
		}
	}
}

func (p *OSCParser) emit(ev Event) {
	p.subsMu.Lock()
	subs := append([]chan Event{}, p.subs...)
	p.subsMu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Feed scans one chunk. base is the absolute offset of p[0].
func (p *OSCParser) Feed(base int64, chunk []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := 0; i < len(chunk); i++ {
		b := chunk[i]
		off := base + int64(i)

		if p.inOSC {
			switch {
			case b == 0x07: // BEL terminator
				p.finish(off + 1)
			case b == 0x1b: // possible ST (ESC \)
				if i+1 < len(chunk) && chunk[i+1] == '\\' {
					i++
					p.finish(off + 2)
				} else if i+1 == len(chunk) {
					// Split exactly at ESC; treat the ESC as a pending
					// terminator candidate handled on the next chunk.
					p.sawEsc = true
				} else {
					// ESC followed by something else: malformed; abort.
					p.reset()
				}
			default:
				if p.sawEsc {
					// Previous chunk ended in ESC.
					p.sawEsc = false
					if b == '\\' {
						p.finish(off + 1)
						continue
					}
					p.reset()
					continue
				}
				p.payload = append(p.payload, b)
				if len(p.payload) > maxOSCPayload {
					p.reset()
				}
			}
			continue
		}

		if p.sawEsc {
			p.sawEsc = false
			if b == ']' {
				p.inOSC = true
				p.payload = p.payload[:0]
				// start was recorded when the ESC was seen
				continue
			}
			continue
		}

		if b == 0x1b {
			p.sawEsc = true
			p.start = off
		}
	}
}

func (p *OSCParser) reset() {
	p.inOSC = false
	p.sawEsc = false
	p.payload = p.payload[:0]
}

func (p *OSCParser) finish(end int64) {
	payload := string(p.payload)
	start := p.start
	p.reset()

	num, rest, _ := strings.Cut(payload, ";")
	switch num {
	case "133":
		code, arg, _ := strings.Cut(rest, ";")
		ev := Event{Start: start, End: end}
		switch code {
		case "A":
			ev.Kind = EvPrompt
		case "B":
			ev.Kind = EvInput
		case "C":
			ev.Kind = EvPreexec
		case "D":
			ev.Kind = EvDone
			ev.Exit, _ = strconv.Atoi(arg)
		default:
			return
		}
		p.emit(ev)
	case "7":
		// file://host/path
		s := strings.TrimPrefix(rest, "file://")
		host, cwd, found := strings.Cut(s, "/")
		if !found {
			return
		}
		p.emit(Event{Kind: EvCwd, Host: host, Cwd: "/" + cwd, Start: start, End: end})
	case "7979":
		nonce, exitStr, _ := strings.Cut(rest, ";")
		exit, err := strconv.Atoi(strings.TrimSpace(exitStr))
		if err != nil {
			return
		}
		p.emit(Event{Kind: EvSentinel, Nonce: nonce, Exit: exit, Start: start, End: end})
	}
}
