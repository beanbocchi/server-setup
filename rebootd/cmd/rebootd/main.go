// rebootd listens for HMAC-signed UDP packets and reboots the machine.
//
// It is built with CGO_ENABLED=0, so it never calls glibc NSS. On 2026-11-22
// every glibc name lookup on this host hung for ten hours and took sshd with it.
//
// Request:  "v1 <action> <unix-seconds> <nonce> <hmac>"
// Reply:    "<text> <hmac>"
// The request hmac is HMAC-SHA256(key, "v1 <action> <unix-seconds> <nonce>").
// The reply hmac is HMAC-SHA256(key, "<nonce> <text>").
//
// Actions: ping (replies with uptime, load, memory, PSI), reboot (systemctl
// reboot, then a sysrq reboot if this process is still alive after -force-after),
// reboot-now (sysrq sync, remount read-only, reboot).
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxSkew  = 60 * time.Second
	nonceLen = 32 // hex characters
)

var (
	listen     = flag.String("listen", ":39017", "UDP address to listen on")
	keyPath    = flag.String("key", "/etc/rebootd/key", "file with the hex-encoded key (at least 32 bytes)")
	forceAfter = flag.Duration("force-after", 90*time.Second, "time to wait for systemctl reboot before the sysrq reboot")
	dryRun     = flag.Bool("dry-run", false, "log reboot actions instead of running them")
)

// Log lines go through a channel and are dropped when it is full. A blocked
// journald must not block the packet loop.
var logCh = make(chan string, 256)

func logf(format string, a ...any) {
	select {
	case logCh <- fmt.Sprintf(format, a...):
	default:
	}
}

func main() {
	flag.Parse()
	go func() {
		for line := range logCh {
			os.Stderr.WriteString(line + "\n")
		}
	}()

	key, err := readKey(*keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rebootd:", err)
		os.Exit(1)
	}

	// Locked pages stay in RAM, so the daemon still answers while the rest of
	// the system is swapping.
	if err := syscall.Mlockall(syscall.MCL_CURRENT | syscall.MCL_FUTURE); err != nil {
		logf("mlockall failed: %v", err)
	}

	// /proc/sysrq-trigger is opened once at startup. A root write to it works
	// whatever kernel.sysrq is set to.
	var sysrq *os.File
	if !*dryRun {
		sysrq, err = os.OpenFile("/proc/sysrq-trigger", os.O_WRONLY, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rebootd: open /proc/sysrq-trigger:", err)
			os.Exit(1)
		}
	}

	conn, err := net.ListenPacket("udp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rebootd:", err)
		os.Exit(1)
	}
	logf("listening on udp %s (dry-run=%v)", *listen, *dryRun)

	r := &rebooter{sysrq: sysrq}
	seen := map[string]time.Time{}
	buf := make([]byte, 512)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			logf("read: %v", err)
			continue
		}
		action, nonce, ok := verify(key, buf[:n], seen)
		if !ok {
			// Invalid packets get no reply, so a port scan learns nothing.
			continue
		}
		logf("%s from %s", action, addr)

		var text string
		switch action {
		case "ping":
			text = "pong " + status()
		case "reboot":
			text = "rebooting " + status()
		case "reboot-now":
			text = "rebooting-now " + status()
		}
		conn.WriteTo([]byte(text+" "+sign(key, nonce+" "+text)), addr)

		switch action {
		case "reboot":
			go r.reboot(false)
		case "reboot-now":
			go r.reboot(true)
		}
	}
}

func readKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("%s: key has %d bytes, need at least 32", path, len(key))
	}
	return key, nil
}

func sign(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func verify(key, pkt []byte, seen map[string]time.Time) (action, nonce string, ok bool) {
	f := strings.Fields(string(pkt))
	if len(f) != 5 || f[0] != "v1" {
		return "", "", false
	}
	switch f[1] {
	case "ping", "reboot", "reboot-now":
	default:
		return "", "", false
	}
	if len(f[3]) != nonceLen {
		return "", "", false
	}
	want := sign(key, strings.Join(f[:4], " "))
	if !hmac.Equal([]byte(want), []byte(f[4])) {
		return "", "", false
	}
	ts, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return "", "", false
	}
	now := time.Now()
	if d := now.Sub(time.Unix(ts, 0)); d > maxSkew || d < -maxSkew {
		logf("rejected %s: clock skew %s", f[1], d.Round(time.Second))
		return "", "", false
	}
	// A nonce is remembered for twice the allowed skew, which covers every
	// timestamp that could still pass the check above.
	for k, t := range seen {
		if now.Sub(t) > 2*maxSkew {
			delete(seen, k)
		}
	}
	if _, dup := seen[f[3]]; dup {
		logf("rejected %s: replayed nonce", f[1])
		return "", "", false
	}
	seen[f[3]] = now
	return f[1], f[3], true
}

type rebooter struct {
	sysrq *os.File
	once  sync.Once
}

// reboot runs at most once per process; a later request still gets a reply.
func (r *rebooter) reboot(now bool) {
	r.once.Do(func() {
		r.trigger('s')
		if !now {
			logf("running systemctl reboot")
			if *dryRun {
				logf("dry-run: skipped systemctl reboot")
			} else {
				cmd := exec.Command("/usr/bin/systemctl", "reboot")
				if err := cmd.Start(); err != nil {
					logf("systemctl reboot: %v", err)
				} else {
					go cmd.Wait()
				}
			}
			// During a normal shutdown systemd sends SIGTERM and this process
			// exits before the timer fires.
			time.Sleep(*forceAfter)
			logf("still running %s after systemctl reboot, using sysrq", *forceAfter)
			r.trigger('s')
		}
		time.Sleep(3 * time.Second)
		r.trigger('u')
		time.Sleep(2 * time.Second)
		r.trigger('b')
	})
}

func (r *rebooter) trigger(c byte) {
	logf("sysrq %c", c)
	if *dryRun {
		return
	}
	if _, err := r.sysrq.Write([]byte{c}); err != nil {
		logf("sysrq %c: %v", c, err)
	}
}

// status reads /proc only, so it cannot hang on NSS or on another daemon.
func status() string {
	var parts []string
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if s, err := strconv.ParseFloat(f[0], 64); err == nil {
				parts = append(parts, "up="+(time.Duration(s)*time.Second).String())
			}
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 3 {
			parts = append(parts, "load="+f[0]+","+f[1]+","+f[2])
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range bytes.Split(b, []byte("\n")) {
			f := strings.Fields(string(line))
			if len(f) >= 2 && (f[0] == "MemAvailable:" || f[0] == "SwapFree:") {
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				name := strings.ToLower(strings.TrimSuffix(f[0], ":"))
				parts = append(parts, fmt.Sprintf("%s=%dM", name, kb/1024))
			}
		}
	}
	if b, err := os.ReadFile("/proc/pressure/memory"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "full ") {
				for _, kv := range strings.Fields(line)[1:] {
					if strings.HasPrefix(kv, "avg60=") {
						parts = append(parts, "mem-full-avg60="+strings.TrimPrefix(kv, "avg60=")+"%")
					}
				}
			}
		}
	}
	return strings.Join(parts, " ")
}
