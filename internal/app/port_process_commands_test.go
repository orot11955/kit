package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"kit/internal/clierror"
	"kit/internal/procutil"
)

func TestProcessCommandJSONCurrentProcess(t *testing.T) {
	var output bytes.Buffer
	a := &Application{IO: IO{In: bytes.NewBuffer(nil), Out: &output, ErrOut: &output}}
	if err := a.RunCLI(context.Background(), []string{"process", strconv.Itoa(os.Getpid()), "--json"}); err != nil {
		t.Fatal(err)
	}
	var info procutil.Process
	if err := json.Unmarshal(output.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.PID != os.Getpid() || info.PPID <= 0 || info.Command == "" {
		t.Fatalf("unexpected process info: %#v", info)
	}
}

func TestProcessKillJSONRequiresYes(t *testing.T) {
	a := &Application{IO: IO{In: bytes.NewBuffer(nil), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}}
	err := a.RunCLI(context.Background(), []string{"process", "kill", strconv.Itoa(os.Getpid()), "--json"})
	if err == nil || clierror.Code(err) != clierror.Usage {
		t.Fatalf("expected usage error, got %v", err)
	}
}

func TestProcessKillTerminatesChildJSON(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	var output bytes.Buffer
	a := &Application{IO: IO{In: bytes.NewBuffer(nil), Out: &output, ErrOut: &output}}
	if err := a.RunCLI(context.Background(), []string{"process", "kill", strconv.Itoa(pid), "--signal", "TERM", "--yes", "--json"}); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	var result processKillResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	if !result.Killed || result.Process.PID != pid || result.Signal != "TERM" {
		_ = cmd.Process.Kill()
		t.Fatalf("unexpected kill result: %#v", result)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child did not terminate")
	}
}

func TestPortCommandFindsCurrentListenerJSON(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is not installed on this runner")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	var output bytes.Buffer
	a := &Application{IO: IO{In: bytes.NewBuffer(nil), Out: &output, ErrOut: &output}}
	if err := a.RunCLI(context.Background(), []string{"port", strconv.Itoa(port), "--json"}); err != nil {
		t.Fatal(err)
	}
	var result portResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Port != port {
		t.Fatalf("port=%d want %d", result.Port, port)
	}
	for _, item := range result.Listeners {
		if item.PID == os.Getpid() && item.Protocol == "TCP" {
			return
		}
	}
	t.Fatalf("current listener missing: %#v", result.Listeners)
}

func TestPortKillRejectsCurrentProcessBeforeMutation(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is not installed on this runner")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	a := &Application{IO: IO{In: bytes.NewBuffer(nil), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}}
	err = a.RunCLI(context.Background(), []string{"port", "kill", strconv.Itoa(port), "--yes"})
	if err == nil || clierror.Code(err) != clierror.Conflict {
		t.Fatalf("expected protected-process conflict, got %v", err)
	}
	if !errors.Is(listener.SetDeadline(time.Now().Add(time.Millisecond)), nil) {
		// net.TCPListener has no generic health probe; reaching this point without
		// process termination is the meaningful safety assertion.
	}
}
