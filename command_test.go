package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The test binary doubles as the command under test via a helper mode, so the
// tests need no external executable and can report exactly what they received.
func TestMain(m *testing.M) {
	if os.Getenv("HOSTRUN_HELPER") == "1" {
		os.Exit(runHelper())
	}
	os.Exit(m.Run())
}

type helperReport struct {
	Args []string `json:"args"`
	Cwd  string   `json:"cwd"`
	Env  string   `json:"env"`
}

func runHelper() int {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "ERR:" + err.Error()
	}
	encoded, _ := json.Marshal(helperReport{
		Args: argsAfterSeparator(os.Args),
		Cwd:  cwd,
		Env:  os.Getenv("HOSTRUN_HELPER_MARKER"),
	})
	fmt.Fprintln(os.Stdout, string(encoded))
	fmt.Fprintln(os.Stderr, "helper-stderr")
	code, _ := strconv.Atoi(os.Getenv("HOSTRUN_HELPER_EXIT_CODE"))
	return code
}

func argsAfterSeparator(argv []string) []string {
	for i, arg := range argv {
		if arg == "--" {
			return argv[i+1:]
		}
	}
	return nil
}

func TestExecutePreservesArgumentsEnvironmentAndResult(t *testing.T) {
	t.Setenv("HOSTRUN_HELPER", "1")
	t.Setenv("HOSTRUN_HELPER_MARKER", "server-env")
	t.Setenv("HOSTRUN_HELPER_EXIT_CODE", "7")

	want := []string{"", "a b", "qu'o\"te", "line1\nline2", "$HOME", "; rm -rf /", "*"}
	stdout, stderr, code, err := execute(os.Args[0], append([]string{"--"}, want...))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}

	var report helperReport
	if err := json.Unmarshal([]byte(strings.SplitN(string(stdout), "\n", 2)[0]), &report); err != nil {
		t.Fatalf("decode helper report: %v (stdout %q)", err, stdout)
	}
	if !reflect.DeepEqual(report.Args, want) {
		t.Errorf("args = %q, want %q", report.Args, want)
	}
	parentCwd, _ := os.Getwd()
	if report.Cwd != parentCwd || report.Env != "server-env" {
		t.Errorf("cwd/env = %q/%q, want %q/server-env", report.Cwd, report.Env, parentCwd)
	}
	if !strings.Contains(string(stderr), "helper-stderr") {
		t.Errorf("stderr = %q, want helper-stderr", stderr)
	}
}

func TestHandlerRejectsDisallowedCommand(t *testing.T) {
	recorder := postExec(t, `{"command":"/bin/sh","args":["-c","rm -rf /"]}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	var payload errorResponse
	if json.Unmarshal(recorder.Body.Bytes(), &payload) != nil || payload.Error != "command is not allowed" {
		t.Errorf("body = %s", recorder.Body)
	}
}

func postExec(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := &execHandler{registry: map[string]string{"echo": "/bin/echo"}}
	request := httptest.NewRequest(http.MethodPost, "/exec", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
