// Command learn runs Audio Courses on its own: the go-learn engine, its player, and the speech sidecar,
// on one Mac. Put it behind `tailscale serve` for HTTPS on the tailnet (needed for the microphone and
// for installing it on a phone).
//
//	learn -addr 127.0.0.1:8790 -data ~/.local/share/go-learn -owner you@example.com -sidecar ./sidecar
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	_ "modernc.org/sqlite"

	learn "github.com/anchoo2kewl/go-learn"
	"github.com/anchoo2kewl/go-learn/voice"
)

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:8790", "listen address (keep it on loopback; expose with tailscale serve)")
	data := flag.String("data", filepath.Join(home, ".local", "share", "go-learn"), "data directory (database, audio)")
	owner := flag.String("owner", "", "who owns requests from this Mac (required): your Tailscale login")
	sidecarDir := flag.String("sidecar", "", "path to go-learn's sidecar/ directory; empty = don't start it")
	sidecarPort := flag.Int("sidecar-port", 8791, "sidecar port on 127.0.0.1")
	ahead := flag.Duration("ahead", 45*time.Minute, "voiced audio to keep ready ahead of the listener")
	research := flag.String("research", "~/elastio,~/projects,~/play", "comma-separated folders the teacher may read (read-only), plus GitHub via gh; '' turns research off")
	tailnetAddr := flag.String("tailnet-addr", "", "also listen here, on the Mac's Tailscale IP (e.g. 100.x.y.z:8790); callers are identified with tailscale whois")
	flag.Parse()

	log := zerolog.New(os.Stderr).With().Timestamp().Str("app", "learn").Logger()
	if *owner == "" {
		log.Fatal().Msg("-owner is required")
	}
	if err := os.MkdirAll(*data, 0o755); err != nil {
		log.Fatal().Err(err).Msg("data dir")
	}

	db, err := sql.Open("sqlite", "file:"+filepath.Join(*data, "learn.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(on)")
	if err != nil {
		log.Fatal().Err(err).Msg("open database")
	}
	db.SetMaxOpenConns(1)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	vc := &voice.Client{URL: "http://127.0.0.1:" + strconv.Itoa(*sidecarPort)}
	if *sidecarDir != "" {
		go superviseSidecar(ctx, log, *sidecarDir, *sidecarPort)
	}

	eng, err := learn.New(ctx, db,
		learn.WithDataDir(*data),
		learn.WithLogger(log),
		learn.WithVoice(vc, vc),
		learn.WithBufferAhead(*ahead),
		learn.WithIdentity(identity(*owner, log)),
		learn.WithResearch(researchDirs(*research, home)),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("start engine")
	}
	go eng.Run(ctx)

	srv := &http.Server{Addr: *addr, Handler: eng.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if *tailnetAddr != "" {
		tsrv := &http.Server{Addr: *tailnetAddr, Handler: eng.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			_ = tsrv.Close()
		}()
		go func() {
			// The Tailscale IP may not exist yet at login; keep trying.
			for ctx.Err() == nil {
				log.Info().Str("addr", *tailnetAddr).Msg("learn listening on the tailnet")
				if err := tsrv.ListenAndServe(); err == http.ErrServerClosed {
					return
				} else if err != nil {
					log.Warn().Err(err).Msg("tailnet listener; retrying")
				}
				time.Sleep(15 * time.Second)
			}
		}()
	}
	log.Info().Str("addr", *addr).Str("data", *data).Msg("learn listening")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal().Err(err).Msg("serve")
	}
}

var tailnet4, tailnet6 = mustCIDR("100.64.0.0/10"), mustCIDR("fd7a:115c:a1e0::/48")

func mustCIDR(s string) *net.IPNet { _, n, _ := net.ParseCIDR(s); return n }

// identity: tailscale serve passes the tailnet user's login; plain loopback requests are the owner;
// direct tailnet callers are looked up with tailscale whois. Anyone else is refused.
func identity(owner string, log zerolog.Logger) func(*http.Request) (string, bool) {
	type hit struct {
		login string
		at    time.Time
	}
	var mu sync.Mutex
	cache := map[string]hit{}
	return func(r *http.Request) (string, bool) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		switch {
		case ip == nil:
			return "", false
		case ip.IsLoopback():
			if u := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); u != "" {
				return u, true
			}
			return owner, true
		case tailnet4.Contains(ip) || tailnet6.Contains(ip):
			mu.Lock()
			h, ok := cache[host]
			mu.Unlock()
			if ok && time.Since(h.at) < 10*time.Minute {
				return h.login, h.login != ""
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "tailscale", "whois", "--json", host).Output()
			var who struct {
				UserProfile struct{ LoginName string }
			}
			login := ""
			if err == nil && json.Unmarshal(out, &who) == nil {
				login = who.UserProfile.LoginName
			} else {
				log.Warn().Err(err).Str("ip", host).Msg("tailscale whois")
			}
			mu.Lock()
			cache[host] = hit{login, time.Now()}
			mu.Unlock()
			return login, login != ""
		}
		return "", false
	}
}

// superviseSidecar keeps the Python speech sidecar running.
func superviseSidecar(ctx context.Context, log zerolog.Logger, dir string, port int) {
	py := filepath.Join(dir, ".venv", "bin", "python")
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, py, "sidecar.py", "--port", strconv.Itoa(port))
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
		stderr, _ := cmd.StderrPipe()
		cmd.Stdout = cmd.Stderr
		started := time.Now()
		if err := cmd.Start(); err != nil {
			log.Error().Err(err).Msg("start sidecar")
		} else {
			log.Info().Int("pid", cmd.Process.Pid).Msg("sidecar started")
			sc := bufio.NewScanner(stderr)
			for sc.Scan() {
				log.Info().Str("component", "sidecar").Msg(sc.Text())
			}
			err := cmd.Wait()
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(err).Msg("sidecar exited")
		}
		if time.Since(started) > time.Minute {
			backoff = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

// researchDirs expands ~ and keeps folders that exist. It never offers the whole home folder: macOS
// guards Desktop, Documents and the like with privacy prompts no one is there to answer.
func researchDirs(spec, home string) []string {
	var out []string
	for _, d := range strings.Split(spec, ",") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if strings.HasPrefix(d, "~/") {
			d = filepath.Join(home, d[2:])
		}
		d = filepath.Clean(d)
		if d == home || d == "/" {
			continue
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	return out
}
