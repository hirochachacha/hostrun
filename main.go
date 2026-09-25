// Command hostrun runs allowlisted commands on a host from a guest VM or
// container. A single binary is both the server and the client.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// execRequest is the JSON body accepted by POST /exec. execResponse carries a
// completed command's result; stdout and stderr are Base64 so arbitrary bytes
// survive JSON. errorResponse reports failures before a command is started.
type execRequest struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "hostrun: missing command")
		printUsage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "-h", "--help":
		printUsage(os.Stdout)
		return 0
	default:
		return runClient(args)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `hostrun - run allowlisted host commands from a guest

Usage:
  hostrun [--server URL] COMMAND [ARG...]
  hostrun serve [--listen ADDRESS] COMMAND...

Client:
  Sends COMMAND and ARG... to a hostrun server as an argument array and
  writes the returned stdout, stderr, and exit code to the local process.
  --server URL   Server to connect to. Defaults to $HOSTRUN_SERVER.

Server:
  Serves allowlisted commands over HTTP. COMMAND is one or more entries:
    NAME              resolve NAME using PATH at startup
    NAME=/abs/path    expose NAME for an absolute executable path
  --listen ADDR    Address to listen on (default: 127.0.0.1:8080).
  Commands run in the foreground with the server's working directory and
  environment; clients cannot choose the executable or add to the allowlist.

Examples:
  hostrun serve qmd git
  hostrun serve --listen 127.0.0.1:9000 qmd=/opt/homebrew/bin/qmd
  HOSTRUN_SERVER=http://host.lima.internal:8080 hostrun qmd search LOGOFF
`)
}

func runClient(args []string) int {
	fs := flag.NewFlagSet("hostrun", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	serverURL := fs.String("server", "", "hostrun server URL")
	fs.Usage = func() { printUsage(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	target := *serverURL
	if target == "" {
		target = os.Getenv("HOSTRUN_SERVER")
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "hostrun: no server specified; use --server or HOSTRUN_SERVER")
		printUsage(os.Stderr)
		return 2
	}

	remote := fs.Args()
	if len(remote) == 0 {
		fmt.Fprintln(os.Stderr, "hostrun: missing command")
		printUsage(os.Stderr)
		return 2
	}

	payload, err := json.Marshal(execRequest{Command: remote[0], Args: remote[1:]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: %v\n", err)
		return 1
	}

	request, err := http.NewRequest(http.MethodPost, strings.TrimRight(target, "/")+"/exec", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: invalid server URL %q: %v\n", target, err)
		return 1
	}
	request.Header.Set("Content-Type", "application/json")

	// A local endpoint is not reachable through a proxy, and a failed request
	// is never retried: the server may already have started the command, so
	// the result is unknown rather than known to have failed.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	response, err := client.Do(request)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: request failed: %v (command may have run; result unknown)\n", err)
		return 1
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return reportServerError(response)
	}

	var result execResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: invalid response: %v\n", err)
		return 1
	}
	stdout, err := base64.StdEncoding.DecodeString(result.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: invalid response: %v\n", err)
		return 1
	}
	stderr, err := base64.StdEncoding.DecodeString(result.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: invalid response: %v\n", err)
		return 1
	}
	if _, err := os.Stdout.Write(stdout); err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: %v\n", err)
		return 1
	}
	if _, err := os.Stderr.Write(stderr); err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: %v\n", err)
		return 1
	}
	return result.ExitCode
}

func reportServerError(response *http.Response) int {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var payload errorResponse
	if json.Unmarshal(body, &payload) != nil || payload.Error == "" {
		fmt.Fprintf(os.Stderr, "hostrun: server error: %s\n", response.Status)
		return 1
	}
	fmt.Fprintf(os.Stderr, "hostrun: server error: %s: %s\n", response.Status, payload.Error)
	return 1
}
