package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const defaultListenAddress = "127.0.0.1:8080"

var commandNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	listen := fs.String("listen", defaultListenAddress, "address to listen on")
	fs.Usage = func() { printUsage(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	specs := fs.Args()
	if len(specs) == 0 {
		fmt.Fprintln(os.Stderr, "hostrun: serve requires at least one command")
		printUsage(os.Stderr)
		return 2
	}
	registry, err := buildRegistry(specs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: %v\n", err)
		return 1
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: %v\n", err)
		return 1
	}

	mux := http.NewServeMux()
	mux.Handle("/exec", &execHandler{
		registry: registry,
		logger:   log.New(os.Stderr, "hostrun: ", log.LstdFlags),
	})
	fmt.Fprintf(os.Stderr, "hostrun: serving %d command(s) on %s\n", len(registry), listener.Addr())
	if err := http.Serve(listener, mux); err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: %v\n", err)
		return 1
	}
	return 0
}

// buildRegistry maps each public name to the executable path it resolves to at
// startup. Duplicate names, invalid names, and unusable paths are fatal.
func buildRegistry(specs []string) (map[string]string, error) {
	registry := make(map[string]string, len(specs))
	for _, spec := range specs {
		name, path, err := resolveSpec(spec)
		if err != nil {
			return nil, err
		}
		if _, exists := registry[name]; exists {
			return nil, fmt.Errorf("duplicate command name %q", name)
		}
		registry[name] = path
	}
	return registry, nil
}

func resolveSpec(spec string) (string, string, error) {
	name, path, hasPath := strings.Cut(spec, "=")
	if err := validateName(name); err != nil {
		return "", "", fmt.Errorf("invalid command name %q: %w", name, err)
	}
	if !hasPath {
		resolved, err := exec.LookPath(name)
		if err != nil {
			return "", "", fmt.Errorf("command %q not found in PATH", name)
		}
		if abs, err := filepath.Abs(resolved); err == nil {
			resolved = abs
		}
		return name, resolved, nil
	}
	if !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("path for command %q must be absolute: %s", name, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", "", fmt.Errorf("command %q: %w", name, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", "", fmt.Errorf("command %q: %s is not executable", name, path)
	}
	return name, path, nil
}

// validateName keeps public names opaque labels, so a request can never be
// mistaken for a file path and the server alone picks the executable.
func validateName(name string) error {
	if !commandNamePattern.MatchString(name) {
		return errors.New("must match [A-Za-z0-9][A-Za-z0-9._-]*")
	}
	return nil
}

type execHandler struct {
	registry map[string]string
	logger   *log.Logger
	nextID   atomic.Uint64
}

func (h *execHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := h.nextID.Add(1)
	started := time.Now()
	status := http.StatusOK
	exitCode := "-"
	var responseError string
	defer func() {
		h.logger.Printf("response id=%d status=%d exit_code=%s duration=%s error=%q",
			id, status, exitCode, time.Since(started), responseError)
	}()
	respondError := func(code int, message string) {
		status = code
		responseError = message
		writeJSONError(w, code, message)
	}

	if r.Method != http.MethodPost {
		h.logRequest(id, r, "", nil)
		w.Header().Set("Allow", http.MethodPost)
		respondError(http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		h.logRequest(id, r, "", nil)
		respondError(http.StatusBadRequest, "Content-Type must be application/json")
		return
	}

	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.logRequest(id, r, "", nil)
		respondError(http.StatusBadRequest, "invalid request body")
		return
	}
	h.logRequest(id, r, req.Command, req.Args)
	if req.Command == "" {
		respondError(http.StatusBadRequest, "command is required")
		return
	}
	path, allowed := h.registry[req.Command]
	if !allowed {
		respondError(http.StatusForbidden, "command is not allowed")
		return
	}

	stdout, stderr, code, err := execute(path, req.Args)
	if err != nil {
		respondError(http.StatusInternalServerError, err.Error())
		return
	}
	exitCode = strconv.Itoa(code)
	writeJSON(w, http.StatusOK, execResponse{
		ExitCode: code,
		Stdout:   base64.StdEncoding.EncodeToString(stdout),
		Stderr:   base64.StdEncoding.EncodeToString(stderr),
	})
}

func (h *execHandler) logRequest(id uint64, r *http.Request, command string, args []string) {
	h.logger.Printf("request id=%d method=%s path=%q command=%q args=%q", id, r.Method, r.URL.Path, command, args)
}

func isJSONContentType(value string) bool {
	if value == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

// execute runs an allowlisted executable with an argument array and no shell.
// It inherits the server's working directory and environment, and stdin is EOF.
func execute(path string, args []string) ([]byte, []byte, int, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(path, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, nil, 0, fmt.Errorf("failed to run command: %w", err)
	}
	code := cmd.ProcessState.ExitCode()
	if code < 0 {
		code = 1 // terminated by a signal
	}
	return stdout.Bytes(), stderr.Bytes(), code, nil
}
