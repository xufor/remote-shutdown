// remote-shutdown: a tiny web page to shut down or reboot this machine.
package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev" // set at build time with -ldflags "-X main.version=..."

//go:embed index.html
var indexHTML []byte

const (
	serviceName = "remote-shutdown"
	installPath = "/usr/local/bin/remote-shutdown"
	unitPath    = "/etc/systemd/system/remote-shutdown.service"
	defaultPort = 8765
)

func main() {
	log.SetFlags(0)
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "install":
		err = install(args)
	case "uninstall":
		err = uninstall()
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("error: %v", err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `remote-shutdown %s - shut down or reboot this machine from a web page

Usage:
  remote-shutdown [serve] [--addr :%d] [--dry-run]   run the web server
  remote-shutdown install [--port %d] [--user NAME]  install and start as a systemd service
  remote-shutdown uninstall                            stop and remove the service
  remote-shutdown version
`, version, defaultPort, defaultPort)
}

// ---------------------------------------------------------------- server

type server struct {
	user     string // account whose password is asked for
	hostname string
	bootID   string
	chkpwd   string // path to unix_chkpwd, "" if unavailable
	noPasswd bool   // sudo works without a password
	dryRun   bool

	mu      sync.Mutex // serialises power requests (and so password guesses)
	stateMu sync.Mutex
	pending string // action in progress: "", "shutdown" or "reboot"
	lastErr string // why the last action failed, if it did
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", fmt.Sprintf(":%d", defaultPort), "address to listen on")
	dryRun := fs.Bool("dry-run", false, "check passwords but do not actually power off or reboot")
	fs.Parse(args)

	u, err := user.Current()
	if err != nil {
		return err
	}
	s := &server{user: u.Username, dryRun: *dryRun}
	s.hostname, _ = os.Hostname()
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		s.bootID = strings.TrimSpace(string(b))
	} else {
		s.bootID = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	for _, p := range []string{"/usr/sbin/unix_chkpwd", "/sbin/unix_chkpwd"} {
		if _, err := os.Stat(p); err == nil {
			s.chkpwd = p
			break
		}
	}
	s.noPasswd = exec.Command("sudo", "-n", "-k", "true").Run() == nil
	if s.chkpwd == "" && s.noPasswd {
		log.Printf("warning: sudo is passwordless and unix_chkpwd is missing, so passwords cannot be verified; power actions will be refused")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/power", s.handlePower)

	log.Printf("remote-shutdown %s listening on %s (password of user %q)", version, *addr, s.user)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var uptime float64
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			uptime, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	s.stateMu.Lock()
	pending, lastErr := s.pending, s.lastErr
	s.stateMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"hostname": s.hostname,
		"user":     s.user,
		"boot_id":  s.bootID,
		"uptime":   int64(uptime),
		"version":  version,
		"pending":  pending,
		"error":    lastErr,
		"dry_run":  s.dryRun,
	})
}

func (s *server) handlePower(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action   string `json:"action"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if req.Action != "shutdown" && req.Action != "reboot" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown action"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.stateMu.Lock()
	busy := s.pending
	s.stateMu.Unlock()
	if busy != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a " + busy + " is already in progress"})
		return
	}

	if err := s.authenticate(req.Password); err != nil {
		time.Sleep(2 * time.Second) // slow down guessing
		log.Printf("%s refused for %s: %v", req.Action, r.RemoteAddr, err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}

	log.Printf("%s requested by %s", req.Action, r.RemoteAddr)
	s.stateMu.Lock()
	s.pending, s.lastErr = req.Action, ""
	s.stateMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"ok": req.Action, "boot_id": s.bootID})

	// Give the response a moment to reach the browser before pulling the plug.
	go func(action, password string) {
		time.Sleep(time.Second)
		err := s.power(action, password)
		if err == nil && !s.dryRun {
			return // the system is going down; systemd will stop us
		}
		s.stateMu.Lock()
		s.pending = ""
		if err != nil {
			s.lastErr = err.Error()
			log.Printf("%s failed: %v", action, err)
		}
		s.stateMu.Unlock()
	}(req.Action, req.Password)
}

var errBadPassword = errors.New("wrong password")

// authenticate checks the password really belongs to the service user and
// that it is enough to run commands through sudo.
func (s *server) authenticate(password string) error {
	if password == "" || strings.ContainsAny(password, "\x00\n") {
		return errBadPassword
	}
	if s.chkpwd != "" {
		// unix_chkpwd lets a user verify their own password (NUL-terminated on stdin).
		cmd := exec.Command(s.chkpwd, s.user, "nonull")
		cmd.Stdin = strings.NewReader(password + "\x00")
		if cmd.Run() != nil {
			return errBadPassword
		}
	} else if s.noPasswd {
		return errors.New("this system cannot verify passwords (passwordless sudo, no unix_chkpwd)")
	}
	if out, err := sudo(password, "true"); err != nil {
		if s.chkpwd == "" {
			return errBadPassword
		}
		return fmt.Errorf("user %s is not allowed to use sudo: %s", s.user, firstLine(out))
	}
	return nil
}

func (s *server) power(action, password string) error {
	verb := map[string]string{"shutdown": "poweroff", "reboot": "reboot"}[action]
	if s.dryRun {
		log.Printf("dry-run: would run: sudo systemctl %s", verb)
		time.Sleep(5 * time.Second)
		return nil
	}
	if out, err := sudo(password, "systemctl", verb); err != nil {
		return fmt.Errorf("systemctl %s: %s", verb, firstLine(out))
	}
	return nil
}

// sudo runs a command as root, feeding the password on stdin.
func sudo(password string, args ...string) (string, error) {
	cmd := exec.Command("sudo", append([]string{"-S", "-k", "-p", "", "--"}, args...)...)
	cmd.Stdin = strings.NewReader(password + "\n")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "command failed"
	}
	return s
}

// ---------------------------------------------------------------- install

const unitTemplate = `[Unit]
Description=Remote Shutdown - shut down or reboot from a web page
After=network-online.target
Wants=network-online.target

[Service]
User=%s
ExecStart=%s serve --addr :%d
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`

// becomeRoot re-runs the current command through sudo when not already root.
func becomeRoot() error {
	if os.Geteuid() == 0 {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("this needs root; run it again as root")
	}
	fmt.Println("Root access is needed; asking sudo...")
	return syscall.Exec(sudoPath, append([]string{"sudo", self}, os.Args[1:]...), os.Environ())
}

func install(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	port := fs.Int("port", defaultPort, "port for the web page")
	userName := fs.String("user", "", "account the service runs as, and whose password the page asks for (default: the user running sudo)")
	fs.Parse(args)

	if err := becomeRoot(); err != nil {
		return err
	}
	if *userName == "" {
		*userName = os.Getenv("SUDO_USER")
	}
	if *userName == "" || *userName == "root" {
		return errors.New("could not tell which user to run as; pass --user NAME (a user with sudo rights)")
	}
	if _, err := user.Lookup(*userName); err != nil {
		return err
	}
	portGiven := false
	fs.Visit(func(f *flag.Flag) { portGiven = portGiven || f.Name == "port" })
	if st, err := os.Stdin.Stat(); !portGiven && err == nil && st.Mode()&os.ModeCharDevice != 0 {
		fmt.Printf("Port for the web page [%d]: ", *port)
		var line string
		fmt.Scanln(&line)
		if line = strings.TrimSpace(line); line != "" {
			n, err := strconv.Atoi(line)
			if err != nil {
				return fmt.Errorf("invalid port %q", line)
			}
			*port = n
		}
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("invalid port %d", *port)
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	if self != installPath {
		data, err := os.ReadFile(self)
		if err != nil {
			return err
		}
		tmp := installPath + ".new"
		if err := os.WriteFile(tmp, data, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(tmp, 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmp, installPath); err != nil {
			return err
		}
		fmt.Println("Installed", installPath)
	}

	unit := fmt.Sprintf(unitTemplate, *userName, installPath, *port)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	fmt.Println("Wrote", unitPath)

	for _, c := range [][]string{{"daemon-reload"}, {"enable", serviceName}, {"restart", serviceName}} {
		if out, err := exec.Command("systemctl", c...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %s", strings.Join(c, " "), firstLine(string(out)))
		}
	}

	time.Sleep(1500 * time.Millisecond)
	if exec.Command("systemctl", "is-active", "--quiet", serviceName).Run() != nil {
		return fmt.Errorf("the service did not stay running (is port %d already in use?); see: journalctl -u %s -n 20", *port, serviceName)
	}

	fmt.Printf("\nService %q is running as %s and starts on boot. Open:\n", serviceName, *userName)
	if h, err := os.Hostname(); err == nil {
		fmt.Printf("  http://%s.local:%d\n", h, *port)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				fmt.Printf("  http://%s:%d\n", ipn.IP, *port)
			}
		}
	}
	fmt.Printf("The page asks for %s's password before shutting down or rebooting.\n", *userName)
	return nil
}

func uninstall() error {
	if err := becomeRoot(); err != nil {
		return err
	}
	exec.Command("systemctl", "disable", "--now", serviceName).Run()
	for _, p := range []string{unitPath, installPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	exec.Command("systemctl", "daemon-reload").Run()
	fmt.Println("remote-shutdown service removed.")
	return nil
}
