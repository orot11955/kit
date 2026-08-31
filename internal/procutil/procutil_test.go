package procutil

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseLSOF(t *testing.T) {
	items := parseLSOF([]byte("p123\ncnode\nLdev\nPTCP\nn127.0.0.1:3000\np456\ncserver\nLops\nPUDP\nn*:3000\n"))
	if len(items) != 2 {
		t.Fatalf("len=%d items=%#v", len(items), items)
	}
	if items[0].PID != 123 || items[0].Command != "node" || items[0].User != "dev" || items[0].Protocol != "TCP" || items[0].Address != "127.0.0.1:3000" {
		t.Fatalf("unexpected first listener: %#v", items[0])
	}
	if items[1].PID != 456 || items[1].Protocol != "UDP" || items[1].Address != "*:3000" {
		t.Fatalf("unexpected second listener: %#v", items[1])
	}
}

func TestInfoCurrentProcess(t *testing.T) {
	info, err := Info(context.Background(), nil, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if info.PID != os.Getpid() || info.PPID <= 0 || info.Command == "" {
		t.Fatalf("unexpected process info: %#v", info)
	}
}

func TestListenersFindCurrentTCPListener(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is not installed on this runner")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	items, err := Listeners(context.Background(), nil, port)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.PID == os.Getpid() && item.Protocol == "TCP" && strings.HasSuffix(item.Address, ":"+strconv.Itoa(port)) {
			return
		}
	}
	t.Fatalf("current TCP listener was not found on port %d: %#v", port, items)
}

func TestSignalTerminatesChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := Signal(pid, 15); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child did not terminate after SIGTERM")
	}
}

func TestParseSignal(t *testing.T) {
	for input, expected := range map[string]string{"": "TERM", "SIGTERM": "TERM", "kill": "KILL", "int": "INT", "hup": "HUP", "quit": "QUIT"} {
		_, name, err := ParseSignal(input)
		if err != nil || name != expected {
			t.Fatalf("ParseSignal(%q)=(%q,%v), want %q", input, name, err, expected)
		}
	}
	if _, _, err := ParseSignal("USR1"); err == nil {
		t.Fatal("expected unsupported signal to fail")
	}
}
