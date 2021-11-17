package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pires/go-proxyproto"

	"git.sr.ht/~emersion/soju"
	"git.sr.ht/~emersion/soju/config"
)

// TCP keep-alive interval for downstream TCP connections
const downstreamKeepAlive = 1 * time.Hour

type stringSliceFlag []string

func (v *stringSliceFlag) String() string {
	return fmt.Sprint([]string(*v))
}

func (v *stringSliceFlag) Set(s string) error {
	*v = append(*v, s)
	return nil
}

func bumpOpenedFileLimit() error {
	var rlimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit); err != nil {
		return fmt.Errorf("failed to get RLIMIT_NOFILE: %v", err)
	}
	rlimit.Cur = rlimit.Max
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlimit); err != nil {
		return fmt.Errorf("failed to set RLIMIT_NOFILE: %v", err)
	}
	return nil
}

var (
	configPath string
	debug      bool

	tlsCert atomic.Value // *tls.Certificate
)

func loadConfig() (*config.Server, *soju.Config, error) {
	var raw *config.Server
	if configPath != "" {
		var err error
		raw, err = config.Load(configPath)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load config file: %v", err)
		}
	} else {
		raw = config.Defaults()
	}

	var motd string
	if raw.MOTDPath != "" {
		b, err := ioutil.ReadFile(raw.MOTDPath)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load MOTD: %v", err)
		}
		motd = strings.TrimSuffix(string(b), "\n")
	}

	if raw.TLS != nil {
		cert, err := tls.LoadX509KeyPair(raw.TLS.CertPath, raw.TLS.KeyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load TLS certificate and key: %v", err)
		}
		tlsCert.Store(&cert)
	}

	cfg := &soju.Config{
		Hostname:        raw.Hostname,
		Title:           raw.Title,
		LogPath:         raw.LogPath,
		HTTPOrigins:     raw.HTTPOrigins,
		AcceptProxyIPs:  raw.AcceptProxyIPs,
		MaxUserNetworks: raw.MaxUserNetworks,
		MultiUpstream:   raw.MultiUpstream,
		Debug:           debug,
		MOTD:            motd,
	}
	return raw, cfg, nil
}

func main() {
	var listen []string
	flag.Var((*stringSliceFlag)(&listen), "listen", "listening address")
	flag.StringVar(&configPath, "config", "", "path to configuration file")
	flag.BoolVar(&debug, "debug", false, "enable debug logging")
	flag.Parse()

	cfg, serverCfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	cfg.Listen = append(cfg.Listen, listen...)
	if len(cfg.Listen) == 0 {
		cfg.Listen = []string{":6697"}
	}

	if err := bumpOpenedFileLimit(); err != nil {
		log.Printf("failed to bump max number of opened files: %v", err)
	}

	db, err := soju.OpenDB(cfg.SQLDriver, cfg.SQLSource)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}

	var tlsCfg *tls.Config
	if cfg.TLS != nil {
		tlsCfg = &tls.Config{
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return tlsCert.Load().(*tls.Certificate), nil
			},
		}
	}

	srv := soju.NewServer(db)
	srv.SetConfig(serverCfg)

	for _, listen := range cfg.Listen {
		listenURI := listen
		if !strings.Contains(listenURI, ":/") {
			// This is a raw domain name, make it an URL with an empty scheme
			listenURI = "//" + listenURI
		}
		u, err := url.Parse(listenURI)
		if err != nil {
			log.Fatalf("failed to parse listen URI %q: %v", listen, err)
		}

		switch u.Scheme {
		case "ircs", "":
			if tlsCfg == nil {
				log.Fatalf("failed to listen on %q: missing TLS configuration", listen)
			}
			host := u.Host
			if _, _, err := net.SplitHostPort(host); err != nil {
				host = host + ":6697"
			}
			ircsTLSCfg := tlsCfg.Clone()
			ircsTLSCfg.NextProtos = []string{"irc"}
			lc := net.ListenConfig{
				KeepAlive: downstreamKeepAlive,
			}
			l, err := lc.Listen(context.Background(), "tcp", host)
			if err != nil {
				log.Fatalf("failed to start TLS listener on %q: %v", listen, err)
			}
			ln := tls.NewListener(l, ircsTLSCfg)
			ln = proxyProtoListener(ln, srv)
			go func() {
				if err := srv.Serve(ln); err != nil {
					log.Printf("serving %q: %v", listen, err)
				}
			}()
		case "irc+insecure":
			host := u.Host
			if _, _, err := net.SplitHostPort(host); err != nil {
				host = host + ":6667"
			}
			lc := net.ListenConfig{
				KeepAlive: downstreamKeepAlive,
			}
			ln, err := lc.Listen(context.Background(), "tcp", host)
			if err != nil {
				log.Fatalf("failed to start listener on %q: %v", listen, err)
			}
			ln = proxyProtoListener(ln, srv)
			go func() {
				if err := srv.Serve(ln); err != nil {
					log.Printf("serving %q: %v", listen, err)
				}
			}()
		case "unix":
			ln, err := net.Listen("unix", u.Path)
			if err != nil {
				log.Fatalf("failed to start listener on %q: %v", listen, err)
			}
			ln = proxyProtoListener(ln, srv)
			go func() {
				if err := srv.Serve(ln); err != nil {
					log.Printf("serving %q: %v", listen, err)
				}
			}()
		case "wss":
			if tlsCfg == nil {
				log.Fatalf("failed to listen on %q: missing TLS configuration", listen)
			}
			addr := u.Host
			if _, _, err := net.SplitHostPort(addr); err != nil {
				addr = addr + ":https"
			}
			httpSrv := http.Server{
				Addr:      addr,
				TLSConfig: tlsCfg,
				Handler:   srv,
			}
			go func() {
				if err := httpSrv.ListenAndServeTLS("", ""); err != nil {
					log.Fatalf("serving %q: %v", listen, err)
				}
			}()
		case "ws+insecure":
			addr := u.Host
			if _, _, err := net.SplitHostPort(addr); err != nil {
				addr = addr + ":http"
			}
			httpSrv := http.Server{
				Addr:    addr,
				Handler: srv,
			}
			go func() {
				if err := httpSrv.ListenAndServe(); err != nil {
					log.Fatalf("serving %q: %v", listen, err)
				}
			}()
		case "ident":
			if srv.Identd == nil {
				srv.Identd = soju.NewIdentd()
			}

			host := u.Host
			if _, _, err := net.SplitHostPort(host); err != nil {
				host = host + ":113"
			}
			ln, err := net.Listen("tcp", host)
			if err != nil {
				log.Fatalf("failed to start listener on %q: %v", listen, err)
			}
			ln = proxyProtoListener(ln, srv)
			go func() {
				if err := srv.Identd.Serve(ln); err != nil {
					log.Printf("serving %q: %v", listen, err)
				}
			}()
		default:
			log.Fatalf("failed to listen on %q: unsupported scheme", listen)
		}

		log.Printf("server listening on %q", listen)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}

	for sig := range sigCh {
		switch sig {
		case syscall.SIGHUP:
			log.Print("reloading configuration")
			_, serverCfg, err := loadConfig()
			if err != nil {
				log.Printf("failed to reloading configuration: %v", err)
			} else {
				srv.SetConfig(serverCfg)
			}
		case syscall.SIGINT, syscall.SIGTERM:
			log.Print("shutting down server")
			srv.Shutdown()
			return
		}
	}
}

func proxyProtoListener(ln net.Listener, srv *soju.Server) net.Listener {
	return &proxyproto.Listener{
		Listener: ln,
		Policy: func(upstream net.Addr) (proxyproto.Policy, error) {
			tcpAddr, ok := upstream.(*net.TCPAddr)
			if !ok {
				return proxyproto.IGNORE, nil
			}
			if srv.Config().AcceptProxyIPs.Contains(tcpAddr.IP) {
				return proxyproto.USE, nil
			}
			return proxyproto.IGNORE, nil
		},
		ReadHeaderTimeout: 5 * time.Second,
	}
}
