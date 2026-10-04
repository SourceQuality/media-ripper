// Command media-ripper watches optical drives and rips what is inserted.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sourcequality/media-ripper/internal/auth"
	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/discordbot"
	"github.com/sourcequality/media-ripper/internal/drive"
	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/pipeline"
	"github.com/sourcequality/media-ripper/internal/store"
	"github.com/sourcequality/media-ripper/internal/tlsutil"
	"github.com/sourcequality/media-ripper/internal/udf"
	"github.com/sourcequality/media-ripper/internal/updates"
	"github.com/sourcequality/media-ripper/internal/web"
)

var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `media-ripper %s

Usage:
  media-ripper run    [-config FILE]            watch drives and rip (default)
  media-ripper scan   [-config FILE] [DEVICE]   dry run: scan, identify, select; no rip
  media-ripper check  [-config FILE]            validate config and required tools
  media-ripper eject  [DEVICE]                  open the tray
  media-ripper label  [DEVICE]                  print the disc label and fingerprint
  media-ripper passwd [-config FILE] [-user NAME]  set the web UI sign-in password
  media-ripper version

Config file: -config FILE, $MR_CONFIG, /etc/media-ripper/config.yaml, ./config.yaml
`, version)
}

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = usage
	cfgPath := fs.String("config", "", "config file")
	jsonOut := fs.Bool("json", false, "json output (scan, label)")
	userName := fs.String("user", "", "user name (passwd)")
	_ = fs.Parse(args)

	var err error
	switch cmd {
	case "run":
		err = runDaemon(*cfgPath)
	case "scan":
		err = runScan(*cfgPath, fs.Arg(0), *jsonOut)
	case "check":
		err = runCheck(*cfgPath)
	case "eject":
		err = runEject(fs.Arg(0))
	case "label":
		err = runLabel(fs.Arg(0), *jsonOut)
	case "passwd":
		err = runPasswd(*cfgPath, *userName)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func findConfig(path string) string {
	if path != "" {
		return path
	}
	if p := os.Getenv("MR_CONFIG"); p != "" {
		return p
	}
	for _, p := range []string{"/etc/media-ripper/config.yaml", "config.yaml"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.Log.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Log.Format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func build(cfgPath string) (*config.Config, *pipeline.Manager, *store.Store, *slog.Logger, error) {
	path := findConfig(cfgPath)
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	log := newLogger(cfg)
	if path != "" {
		log.Info("config loaded", "file", path)
	}
	st, err := store.Open(cfg.StateDir())
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("open state: %w", err)
	}
	m := pipeline.New(pipeline.Deps{Config: cfg, Store: st, Logger: log})
	return cfg, m, st, log, nil
}

func runDaemon(cfgPath string) error {
	cfg, m, st, log, err := build(cfgPath)
	if err != nil {
		return err
	}
	if err := checkTools(cfg, log); err != nil {
		log.Error("startup check failed", "err", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Web.Enabled {
		watcher := &updates.Watcher{Current: version, Logger: log, Settings: func() updates.Settings {
			u := m.Config().Updates
			return updates.Settings{Check: u.Check, Repo: u.Repo, Token: u.Token}
		}}
		go watcher.Run(ctx)
		srv := &web.Server{Manager: m, Store: st, Version: version, Logger: log, Updates: watcher}
		hs := &http.Server{Addr: cfg.Web.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
		ln, err := net.Listen("tcp", cfg.Web.Listen)
		if err != nil {
			return fmt.Errorf("listen %s: %w", cfg.Web.Listen, err)
		}
		servers := []*http.Server{hs}
		if cfg.Web.TLS == "off" {
			go serve(hs, ln, log)
			log.Info("web ui", "listen", cfg.Web.Listen, "tls", "off")
		} else {
			cert, err := webCertificate(cfg)
			if err != nil {
				return err
			}
			srv.TLS = &web.TLSInfo{Mode: cfg.Web.TLS, Fingerprint: tlsutil.Fingerprint(cert), Expires: tlsutil.Expiry(cert)}
			tlsL, plainL := tlsutil.Split(ln)
			hs.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			// Plain http:// on the same port goes to https://.
			redirect := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusPermanentRedirect)
			})}
			servers = append(servers, redirect)
			go serve(hs, tls.NewListener(tlsL, hs.TLSConfig), log)
			go serve(redirect, plainL, log)
			log.Info("web ui", "listen", cfg.Web.Listen, "tls", cfg.Web.TLS, "fingerprint", srv.TLS.Fingerprint, "expires", srv.TLS.Expires.Format("2006-01-02"))
		}
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for _, s := range servers {
				_ = s.Shutdown(sctx)
			}
		}()
	}
	// Buttons on Discord messages arrive over the bot's Gateway connection.
	if d := cfg.Notify.Discord; d.Buttons && d.BotToken != "" {
		bot := &discordbot.Bot{Token: d.BotToken, AppID: d.ApplicationID, Allowed: d.AllowedUsers, Logger: log,
			Handle: func(ctx context.Context, in discordbot.Interaction) string {
				log.Info("discord button", "action", in.CustomID, "user", in.UserName)
				return m.DiscordAction(ctx, in.CustomID)
			}}
		go bot.Run(ctx)
	}
	log.Info("media-ripper started", "version", version, "output", cfg.Output.Path, "drives", cfg.Drives)
	err = m.Run(ctx)
	if errors.Is(err, context.Canceled) {
		log.Info("stopped")
		return nil
	}
	return err
}

func checkTools(cfg *config.Config, log *slog.Logger) error {
	var errs []error
	if _, err := exec.LookPath(cfg.MakeMKV.Binary); err != nil {
		errs = append(errs, fmt.Errorf("%s not found", cfg.MakeMKV.Binary))
	} else {
		// Check the key the daemon would use, not whatever is on disk now.
		if cfg.MakeMKV.WriteSettings {
			if err := makemkv.WriteSettings(cfg.MakeMKV.SettingsDir, cfg.MakeMKV.Key, makemkv.SelectionString(cfg.Selection.Languages)); err != nil {
				log.Warn("write makemkv settings", "err", err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		version, err := (&makemkv.Client{Binary: cfg.MakeMKV.Binary}).Probe(ctx)
		cancel()
		if err != nil {
			errs = append(errs, err)
		} else {
			fmt.Printf("makemkv: v%s\n", version)
		}
	}
	if cfg.PostProcess.Mode == "remux" {
		_, e1 := exec.LookPath("mkvmerge")
		_, e2 := exec.LookPath("ffmpeg")
		if e1 != nil && e2 != nil {
			log.Warn("postprocess.mode is remux but neither mkvmerge nor ffmpeg is installed; files will be delivered as MakeMKV wrote them")
		}
	}
	if st, err := os.Stat(cfg.Output.Path); err != nil || !st.IsDir() {
		log.Warn("output.path is not an existing directory yet", "path", cfg.Output.Path)
	}
	if err := os.MkdirAll(cfg.Workspace, 0o775); err != nil {
		errs = append(errs, fmt.Errorf("workspace: %w", err))
	}
	return errors.Join(errs...)
}

func runCheck(cfgPath string) error {
	cfg, _, _, log, err := build(cfgPath)
	if err != nil {
		return err
	}
	if err := checkTools(cfg, log); err != nil {
		return err
	}
	drives := cfg.Drives
	if len(drives) == 0 {
		drives = drive.Discover()
	}
	for _, p := range drives {
		d, err := drive.Open(p)
		if err != nil {
			fmt.Printf("%s: %v\n", p, err)
			continue
		}
		st, err := d.Status()
		if err != nil {
			fmt.Printf("%s: %v\n", p, err)
			continue
		}
		fmt.Printf("%s: %s\n", p, st)
	}
	if len(drives) == 0 {
		fmt.Println("no optical drives found")
	}
	fmt.Println("ok")
	return nil
}

func runScan(cfgPath, device string, asJSON bool) error {
	_, m, _, _, err := build(cfgPath)
	if err != nil {
		return err
	}
	device = pickDevice(device)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	job, err := m.ScanOnly(ctx, device)
	if job != nil {
		snap := job.Snapshot()
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(snap)
		} else {
			for _, l := range snap.Log {
				fmt.Println(l.Message)
			}
		}
	}
	return err
}

func runEject(device string) error {
	d, err := drive.Open(pickDevice(device))
	if err != nil {
		return err
	}
	return d.Eject()
}

func runLabel(device string, asJSON bool) error {
	d, err := drive.Open(pickDevice(device))
	if err != nil {
		return err
	}
	fp, label, err := d.Fingerprint()
	if err != nil {
		return err
	}
	hint := metadata.ParseLabel(label)
	// TheDiscDB's content hash, from the file sizes on the disc.
	var hash, hashErr string
	if files, err := udf.ListDevice(pickDevice(device)); err != nil {
		hashErr = err.Error()
	} else if h, err := udf.ContentHash(files); err != nil {
		hashErr = err.Error()
	} else {
		hash = h
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"fingerprint": fp, "label": label, "hint": hint, "content_hash": hash})
	}
	fmt.Printf("label: %s\nfingerprint: %s\nquery: %s\n", label, fp, hint.Query)
	if hash != "" {
		fmt.Printf("content hash: %s\n", hash)
	} else {
		fmt.Printf("content hash: unavailable (%s)\n", hashErr)
	}
	if hint.Year > 0 {
		fmt.Printf("year: %d\n", hint.Year)
	}
	if hint.Season > 0 {
		fmt.Printf("season: %d\n", hint.Season)
	}
	if hint.Disc > 0 {
		fmt.Printf("disc: %d\n", hint.Disc)
	}
	return nil
}

func pickDevice(device string) string {
	if device != "" {
		return device
	}
	if d := drive.Discover(); len(d) > 0 {
		return d[0]
	}
	return "/dev/sr0"
}

// runPasswd sets the web UI account from the command line, for the first
// account on a headless install or when the password is lost. The
// password is read from the terminal without echo, or from stdin.
func runPasswd(cfgPath, user string) error {
	path := findConfig(cfgPath)
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if user == "" {
		user = cfg.Auth.Username
	}
	if user == "" {
		return errors.New("no account yet: give the user name with -user NAME")
	}
	pw, err := readPassword("New password for " + user + ": ")
	if err != nil {
		return err
	}
	if term := isTerminal(); term {
		again, err := readPassword("Again: ")
		if err != nil {
			return err
		}
		if again != pw {
			return errors.New("the passwords do not match")
		}
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	cfg.Auth.Username, cfg.Auth.PasswordHash, cfg.Auth.Enabled = user, hash, true
	if err := cfg.Save(path); err != nil {
		return err
	}
	fmt.Printf("Password set for %s in %s. A running service picks it up after: systemctl restart media-ripper\n", user, path)
	return nil
}

func isTerminal() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// readPassword reads one line; on a terminal the typing is hidden (stty).
func readPassword(prompt string) (string, error) {
	if isTerminal() {
		fmt.Fprint(os.Stderr, prompt)
		off := exec.Command("stty", "-echo")
		off.Stdin = os.Stdin
		if off.Run() == nil {
			defer func() {
				on := exec.Command("stty", "echo")
				on.Stdin = os.Stdin
				_ = on.Run()
				fmt.Fprintln(os.Stderr)
			}()
		}
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no password given")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func serve(hs *http.Server, l net.Listener, log *slog.Logger) {
	if err := hs.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		log.Error("http server", "err", err)
	}
}

// webCertificate is the HTTPS certificate: your own files, or one made on
// this machine and kept with the state (renewed before it expires).
func webCertificate(cfg *config.Config) (tls.Certificate, error) {
	if cfg.Web.TLS == "files" {
		return tlsutil.Load(cfg.Web.TLSCert, cfg.Web.TLSKey)
	}
	return tlsutil.SelfSigned(filepath.Join(cfg.StateDir(), "tls"), tlsutil.Names(cfg.Web.TLSHosts), time.Now())
}
