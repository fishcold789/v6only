//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Microsoft/go-winio"
	core "github.com/y38501148-max/v6only/core"
	"golang.org/x/sys/windows/svc"
	"syscall"
)

const pipeName = `\\.\pipe\v6only-desktop`

type request struct {
	Action        string `json:"action"`
	From          int64  `json:"from"`
	To            int64  `json:"to"`
	URL           string `json:"url"`
	AllowFallback bool   `json:"allow_fallback"`
}

var operation sync.Mutex
var root string
var stopping atomic.Bool

func controller(action string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(root, "desktop-controller.ps1"), "-Action", action)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	// A background core may inherit a PowerShell output handle. Do not wait
	// indefinitely for that descendant after the controller itself has exited.
	cmd.WaitDelay = 2 * time.Second
	b, e := cmd.CombinedOutput()
	if errors.Is(e, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		e = nil
	}
	if e != nil {
		return nil, fmt.Errorf("%s: %w", b, e)
	}
	if !json.Valid(b) {
		if (action == "watch" || action == "stop") && len(strings.TrimSpace(string(b))) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return nil, fmt.Errorf("invalid controller response: %s", b)
	}
	return json.RawMessage(b), nil
}
func handle(q request) (any, error) {
	switch q.Action {
	case "status":
		// Status reads atomic snapshots and never waits behind network mutation.
		return controller("status")
	case "enable", "disable", "set_fallback":
		if stopping.Load() {
			return nil, fmt.Errorf("SERVICE_UNAVAILABLE: 后台服务正在停止")
		}
		return withOperation(&operation, func() (any, error) {
			action := q.Action
			if action == "set_fallback" {
				if q.AllowFallback {
					action = "fallback-on"
				} else {
					action = "fallback-off"
				}
			}
			b, e := controller(action)
			return map[string]any{"status": b}, e
		})
	case "stats":
		if q.To == 0 {
			q.To = time.Now().Unix() + 1
		}
		return core.ReadTraffic(filepath.Join(os.Getenv("ProgramData"), "v6only", "traffic.sqlite"), q.From, q.To)
	case "diagnose":
		bstatus, e := controller("status")
		if e != nil {
			return nil, e
		}
		var st struct {
			Interface     string   `json:"interface_name"`
			DNS           []string `json:"dns"`
			AllowFallback bool     `json:"allow_ipv4_fallback"`
		}
		json.Unmarshal(bstatus, &st)
		if st.Interface == "" {
			return nil, fmt.Errorf("No physical network adapter")
		}
		if len(st.DNS) == 0 {
			st.DNS = []string{"223.5.5.5", "223.6.6.6"}
		}
		raw := q.URL
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		u, e := url.Parse(raw)
		if e != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return nil, fmt.Errorf("请输入有效的 HTTP/HTTPS 地址")
		}
		// Only a URL is accepted; it is passed as one argv value, never to a shell.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		args := []string{"--interface", st.Interface, "--dns", strings.Join(st.DNS, ","), "--ipv6-dns", "2400:3200::1,2400:3200:baba::1", "--chatgpt-ipv4", "--probe", raw}
		if st.AllowFallback {
			args = append(args, "--allow-ipv4-fallback")
		}
		cmd := exec.CommandContext(ctx, filepath.Join(root, "v6core.exe"), args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		b, e := cmd.Output()
		if !json.Valid(b) {
			return nil, e
		}
		var r any
		e = json.Unmarshal(b, &r)
		return r, e
	default:
		return nil, fmt.Errorf("unknown action")
	}
}
func serve(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	s := bufio.NewScanner(io.LimitReader(c, 16384))
	if !s.Scan() {
		return
	}
	var q request
	if json.Unmarshal(s.Bytes(), &q) != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	c.SetWriteDeadline(time.Now().Add(85 * time.Second))
	data, e := handle(q)
	r := map[string]any{"ok": e == nil, "data": data}
	if e != nil {
		r["error"] = e.Error()
	}
	json.NewEncoder(c).Encode(r)
}

type service struct{}

func (service) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	// Deny network logons. Local interactive users may invoke only the fixed RPC actions.
	l, e := winio.ListenPipe(pipeName, &winio.PipeConfig{SecurityDescriptor: "D:P(D;;GA;;;NU)(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)", InputBufferSize: 65536, OutputBufferSize: 65536})
	if e != nil {
		return false, 1
	}
	defer l.Close()
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go serve(c)
		}
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	done := make(chan struct{})
	watchDone := make(chan struct{})
	defer func() {
		select {
		case <-done:
		default:
			close(done)
		}
	}()
	go func() {
		defer close(watchDone)
		for {
			select {
			case <-done:
				return
			default:
			}
			if operation.TryLock() {
				if _, err := controller("watch"); err != nil {
					log.Printf("watch failed: %v", err)
				}
				operation.Unlock()
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	for r := range requests {
		switch r.Cmd {
		case svc.Interrogate:
			status <- r.CurrentStatus
		case svc.Stop, svc.Shutdown:
			stopping.Store(true)
			l.Close()
			status <- svc.Status{State: svc.StopPending, WaitHint: 180000, CheckPoint: 1}
			close(done)
			cleanup := make(chan error, 1)
			go func() {
				<-watchDone
				operation.Lock()
				defer operation.Unlock()
				_, err := controller("stop")
				cleanup <- err
			}()
			checkpoint := uint32(1)
			var stopErr error
		waitCleanup:
			for {
				select {
				case stopErr = <-cleanup:
					break waitCleanup
				case <-time.After(2 * time.Second):
					checkpoint++
					status <- svc.Status{State: svc.StopPending, WaitHint: 180000, CheckPoint: checkpoint}
				}
			}
			if stopErr != nil {
				log.Printf("stop cleanup failed: %v", stopErr)
			}
			if stopErr != nil {
				return false, 1
			}
			return false, 0
		}
	}
	return false, 0
}
func main() {
	exe, e := os.Executable()
	if e != nil {
		panic(e)
	}
	root = filepath.Join(filepath.Dir(exe), "windows")
	logDir := filepath.Join(os.Getenv("ProgramData"), "v6only")
	if os.MkdirAll(logDir, 0700) == nil {
		if f, err := os.OpenFile(filepath.Join(logDir, "service.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
			defer f.Close()
			log.SetOutput(f)
		}
	}
	if e = svc.Run("V6Only", service{}); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
