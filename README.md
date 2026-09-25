# hostrun

Run allowlisted commands on a host machine from a guest VM or container over
HTTP.

A single binary contains both sides:

- **server** — runs on the host and serves a fixed allowlist of executables.
- **client** — runs in the guest and invokes one of them.

The client can only ask for a public name that the server registered at
startup. It cannot choose an executable path, and commands never go through a
shell.

## Install

```sh
go install github.com/hirochachacha/hostrun@latest
```

The binary is installed to `$(go env GOPATH)/bin` (usually `~/go/bin`). Make
sure that directory is on your `PATH`.

## Build

```sh
go build -o hostrun .
```

Requires Go 1.23 or newer. The code has no platform-specific parts and builds
on Linux, macOS, and Windows.

## Server

```text
hostrun serve [--listen ADDRESS] COMMAND...
```

```sh
# Resolve executables from PATH at startup.
hostrun serve qmd git

# Map a public name to an explicit path.
hostrun serve qmd=/opt/homebrew/bin/qmd git=/usr/bin/git

# Change the listen address (default 127.0.0.1:8080).
hostrun serve --listen 127.0.0.1:9000 qmd
```

Each `COMMAND` is either:

- `NAME` — resolve `NAME` using the server's `PATH` at startup, or
- `NAME=/absolute/path` — expose `NAME` for an absolute executable path.

The public `NAME` is an opaque identifier, never a file path. Duplicate names,
invalid names, and missing or non-executable files are startup errors. The
server runs in the foreground; it does not daemonize.

For each `/exec` request, the server logs the method, path, command name, and
arguments, then the HTTP status, command exit code (if run), elapsed time,
and any error. Each pair shares an ID so concurrent requests can be matched.
Logs go to the server's stderr and do not include command output.

## Client

```text
hostrun [--server URL] COMMAND [ARG...]
```

```sh
hostrun --server http://host.lima.internal:8080 qmd search "LOGOFF" -c ms-specs

export HOSTRUN_SERVER=http://host.lima.internal:8080
hostrun qmd get '#0bd25e'
```

The server URL comes from `--server`, then `HOSTRUN_SERVER`. If neither is set,
the client prints usage and exits non-zero. Everything after the flags is the
remote command and its arguments; `hostrun qmd --help` passes `--help` to the
host's `qmd`. `serve` is reserved for starting a server.

## Transparent usage in the guest

Callers in the guest can keep using a command by its normal name. Put a small
wrapper earlier in `PATH` that forwards to `hostrun`, and nothing else in the
guest needs to know the command actually runs on the host.

```sh
# ~/.local/bin/qmd  (guest)
#!/bin/sh
export HOSTRUN_SERVER=http://host.lima.internal:8080
exec hostrun qmd "$@"
```

```sh
chmod +x ~/.local/bin/qmd
# Make sure ~/.local/bin comes before any locally installed qmd.
```

Now `qmd search "LOGOFF"` in the guest runs `qmd` on the host. An interactive
shell can use an alias instead, but a wrapper also works for non-interactive
callers such as agents and build tools:

```sh
alias qmd='hostrun qmd'
```

### Narrowing a command on the host

hostrun exposes an executable as a whole and leaves its arguments unrestricted.
To allow only some subcommands, register a wrapper on the host and keep the
same public name, so guests are unaffected.

```sh
# /usr/local/bin/qmd-readonly  (host)
#!/bin/sh
set -eu
case "${1-}" in
  query|get) ;;
  *) echo "qmd-readonly: only 'query' and 'get' are allowed" >&2; exit 64 ;;
esac
exec /usr/bin/qmd "$@"
```

```sh
hostrun serve qmd=/usr/local/bin/qmd-readonly
```

Parsing subcommands and options is deliberately left to such a wrapper, where
it can be written with knowledge of the real command and tested on its own.

## Behavior

- The server maps a public name to a registered executable and runs it. No
  shell is involved; the executable and argument array are used directly.
- Arguments are transferred as an array, not joined into a string. Empty
  strings, whitespace, quotes, and newlines are preserved literally.
- A command's arguments are unrestricted: allowing a command also allows every
  function it offers, including changing or deleting files.
- The child inherits the server's working directory and environment. The client
  cannot change either.
- The child's stdin is EOF. Interactive input is not supported.
- After the command finishes, stdout, stderr, and the exit code are returned.
  The client writes each stream to the matching local output and exits with the
  remote exit code. Server logs are never mixed into command output.
- The client never retries a request. On a communication failure the command
  may already have run, so the result is reported as unknown.

## HTTP API

```http
POST /exec
Content-Type: application/json
```

```json
{
  "command": "qmd",
  "args": ["search", "LOGOFF", "-c", "ms-specs"]
}
```

A completed command returns HTTP 200 even when its exit code is non-zero.
`stdout` and `stderr` are Base64 so arbitrary bytes survive the transport.

```json
{
  "exit_code": 0,
  "stdout": "...",
  "stderr": "..."
}
```

Failures that happen before a command can run are reported separately:

| Status | Meaning                          |
| ------ | -------------------------------- |
| 400    | malformed request                |
| 403    | command not allowed              |
| 500    | failed to start or run the command |

```json
{
  "error": "command is not allowed"
}
```

## Security model

This is intended for a **trusted** host and guest. The default listen address
is loopback, and there is no authentication or TLS.

There are deliberately no timeouts, output or request size limits, or
concurrency limits: they were removed for simplicity under the trusted-host
assumption. A long-running command holds its request until it finishes, and
large output is buffered in memory.

## Development

```sh
go test ./...
```
