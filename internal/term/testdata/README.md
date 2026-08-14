# Real terminal capture corpus

Byte-for-byte recordings of what real programs emit to a real PTY, used by
`TestCorpusRealCaptures` to *measure* linearization's blast radius on ordinary
Linux sessions rather than reason about it.

Synthetic fragments are not a substitute: the whole risk being guarded against
is that a real terminal emits something we did not think to imagine.

## How they were captured

Under `script(1)`, which allocates a real PTY so each program uses the escape
repertoire it would for a human (piping to a file instead would suppress most
of it):

```sh
export TERM=xterm-256color PAGER=cat GIT_PAGER=cat
script -qec "git --no-pager log --color=always --graph --oneline -25" git-log.bin
script -qec "ls --color=always -la /etc"                              ls-color.bin
script -qec "grep --color=always -rn linearize internal/term/linearize.go" grep-color.bin
script -qec "top -n 2 -d 0.3"                                         top-frames.bin
script -qec "timeout 2 htop -d 5"                                     htop-frame.bin
script -qec "vim -c 'sleep 400m' -c 'q!' /etc/hostname"               vim-session.bin
```

Two hazards if you regenerate these: anything that pages (git) blocks forever
on a PTY because `less` waits for input — hence `GIT_PAGER=cat` — and any
capture can hang, so wrap each in `timeout`.

## What each one is for

| File | Buffer | Vertical movement | Role |
|---|---|---|---|
| `git-log.bin` | normal | none | SGR-heavy output must be byte-identical |
| `ls-color.bin` | normal | none | ditto |
| `grep-color.bin` | normal | none | ditto |
| `top-frames.bin` | alt | yes | absolute positioning, two redraws |
| `htop-frame.bin` | alt | yes | absolute positioning |
| `vim-session.bin` | alt | yes | alt-screen enter/exit around a redraw |

The first three are the load-bearing ones: they are what an ordinary shell
session looks like, and the no-op property proven over them is the evidence
that existing Linux users are unaffected.

## Known gap

No capture of **multi-line progress in the normal buffer** — the `docker pull`
or `npm install` pattern that moves the cursor *up* with `ESC[nA` to rewrite
earlier lines. That is distinct from a full-screen TUI: it lands in scrollback
and comes back through `read_output`, where the TUI cases mostly do not
(`framing.Run` refuses outright while the alt screen is active).

It was skipped because no Docker daemon was reachable when the corpus was
built. Worth adding when one is:

```sh
docker rmi -f alpine:3.19
script -qec "docker pull alpine:3.19" docker-pull.bin
```
