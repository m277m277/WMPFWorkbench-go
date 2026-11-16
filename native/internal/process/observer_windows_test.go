//go:build windows

package process

import (
	"bufio"
	"context"
	"encoding/json"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestObserverCrashKillsOwnedTree(t *testing.T) {
	exe := os.Getenv("WMPF_TEST_OBSERVER")
	if exe == "" {
		t.Skip("native worker not configured")
	}
	dir := t.TempDir()
	gate := filepath.Join(dir, "go")
	raw, _ := json.Marshal(map[string]any{"host": exe, "appid": "wx0123456789abcdef", "out": dir, "gate_file": gate, "stop_file": filepath.Join(dir, "stop"), "seconds": 30, "fixture": true})
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, raw, 0600)
	cmd := exec.Command(exe, "--config", path)
	pipe, _ := cmd.StdoutPipe()
	p, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ctx, c := context.WithTimeout(context.Background(), 3*time.Second); defer c(); p.Stop(ctx) }()
	os.WriteFile(gate, nil, 0600)
	pids := make(chan uint32, 16)
	go func() {
		s := bufio.NewScanner(pipe)
		for s.Scan() {
			var r struct {
				Event string
				PID   uint32
			}
			json.Unmarshal(s.Bytes(), &r)
			if r.Event == "session.attached" {
				pids <- r.PID
			}
		}
	}()
	handles := []windows.Handle{}
	defer func() {
		for _, h := range handles {
			windows.CloseHandle(h)
		}
	}()
	for len(handles) < 2 {
		select {
		case pid := <-pids:
			h, e := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
			if e != nil {
				t.Fatal(e)
			}
			handles = append(handles, h)
		case <-time.After(15 * time.Second):
			t.Fatal("fixture child not attached")
		}
	}
	// Bypass graceful shutdown, emulating a hard worker crash.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("worker survived kill")
	}
	for _, h := range handles {
		state, e := windows.WaitForSingleObject(h, 5000)
		if e != nil || state != windows.WAIT_OBJECT_0 {
			t.Fatalf("owned process survived worker crash: state=%v err=%v", state, e)
		}
	}
}
