package proxy

import (
	log "github.com/Sirupsen/logrus"

	ctx "context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

func init() {
	LogLevel(os.Getenv("LOG_LEVEL"))
}

// LogLevel sets logging level of go-proxy runtime
func LogLevel(level string) {
	switch level {
	case "DEBUG":
		log.SetLevel(log.DebugLevel)
		break
	case "INFO":
		log.SetLevel(log.InfoLevel)
		break
	case "WARNING":
		log.SetLevel(log.WarnLevel)
		break
	case "ERROR":
		log.SetLevel(log.ErrorLevel)
		break
	case "FATAL":
		log.SetLevel(log.FatalLevel)
		break
	case "PANIC":
		log.SetLevel(log.PanicLevel)
		break
	default:
		log.SetLevel(log.InfoLevel)
		break
	}
}

var (
	// Proxy session ended.
	ErrProxyEnd = errors.New("proxy end")

	// Requested cluster node, but source and destination count does not match
	ErrClusterNodeMismatch = errors.New("Origin and target count mismatch")

	// Requested cluster mode, but I cannot find enough nodes to cluster to
	ErrClusterNotEnoughNodes = errors.New("candidate less then asked")
)

type TLSConfig struct {
	// If non nil Client enables sending of TLS encrypted data
	Client *tls.Config

	// If non nil Server enables receiving TLS encrypted data
	Server *tls.Config
}

// ConnOptions defines how the proxy should behave
type ConnOptions struct {
	// Type of network transport
	// See https://godoc.org/net
	Net string

	// Listening origin
	From string
	// Listening origin list for cluster mode
	FromRange []string

	// Balacnce forwarding host using round robin
	Balance bool
	// List of forwarding host
	To []string

	// TLS config
	TLSConfig TLSConfig

	// Discovery backend setting
	Discovery *DiscOptions

	// Read timeout
	ReadTimeout time.Duration

	// Write timeout
	WriteTimeout time.Duration
}

type DiscOptions struct {
	// Service key to registered host
	Service string

	// Discovery host netloc
	Endpoints []string

	// etcd index for event propagation
	// See https://godoc.org/github.com/coreos/etcd/client#WatcherOptions
	AfterIndex uint64
}

func runTo(newConn <-chan net.Conn, c ctx.Context, opts *ConnOptions) {
	for yay := true; yay; {
		select {
		case conn := <-newConn:
			work, _ := ctx.WithCancel(c)
			go handleConn(work, &connOrder{
				conn,
				opts.Net,
				opts.To,
				opts.ReadTimeout,
				opts.WriteTimeout,
				opts.TLSConfig.Client,
			})
		case <-c.Done():
			yay = false
		}
	}
}

func balanceTo(newConn <-chan net.Conn, c ctx.Context, opts *ConnOptions) {
	for yay, r := true, 0; yay; r = (r + 1) % len(opts.To) {
		select {
		case conn := <-newConn:
			work, _ := ctx.WithCancel(c)
			go handleConn(work, &connOrder{
				conn,
				opts.Net,
				opts.To[r : r+1],
				opts.ReadTimeout,
				opts.WriteTimeout,
				opts.TLSConfig.Client,
			})
		case <-c.Done():
			yay = false
		}
	}
}

// To takes a Context and ConnOptions and begin listening for request to
// proxy.
// To obtains origin candidates through static listing.
// Review https://godoc.org/golang.org/x/net/context for understanding the
// control flow.
func To(c ctx.Context, opts *ConnOptions) error {
	newConn, astp, err := acceptWorker(c, &config{
		opts.Net,
		opts.From,
		opts.TLSConfig.Server,
	})
	if err != nil {
		return err // something bad happend to Accepter
	}
	defer func() { <-astp }()

	log.WithFields(log.Fields{"from": opts.From}).Debug("TO start")
	if opts.Balance {
		balanceTo(newConn, c, opts)
	} else {
		runTo(newConn, c, opts)
	}
	log.WithFields(log.Fields{"from": opts.From}).Debug("TO stop")
	return ErrProxyEnd
}

func runSrv(newConn <-chan net.Conn, newNodes <-chan []string, c ctx.Context, opts *ConnOptions) {
	var connList = make([]ctx.CancelFunc, 0)
	for yay := true; yay; {
		select {
		case nodes := <-newNodes:
			if nodes != nil {
				opts.To = nodes
				// TODO: memory efficient way of doing this?
				for _, abort := range connList {
					abort()
				}
				connList = make([]ctx.CancelFunc, 0)
			}
		case conn := <-newConn:
			if len(opts.To) == 0 {
				conn.Close() // close connection to avoid confusion
			} else {
				work, abort := ctx.WithCancel(c)
				go handleConn(work, &connOrder{
					conn,
					opts.Net,
					opts.To,
					opts.ReadTimeout,
					opts.WriteTimeout,
					opts.TLSConfig.Client,
				})
				connList = append(connList, abort)
			}
		case <-c.Done():
			yay = false
		}
	}
}

func balacnceSrv(newConn <-chan net.Conn, newNodes <-chan []string, c ctx.Context, opts *ConnOptions) {
	var connList = make([]ctx.CancelFunc, 0)
	for yay, r := true, 0; yay; r = (r + 1) % len(opts.To) {
		select {
		case nodes := <-newNodes:
			if nodes != nil {
				opts.To = nodes
				// TODO: memory efficient way of doing this?
				for _, abort := range connList {
					abort()
				}
				connList = make([]ctx.CancelFunc, 0)
			}
		case conn := <-newConn:
			if len(opts.To) == 0 {
				conn.Close() // close connection to avoid confusion
			} else {
				work, abort := ctx.WithCancel(c)
				go handleConn(work, &connOrder{
					conn,
					opts.Net,
					opts.To[r : r+1],
					opts.ReadTimeout,
					opts.WriteTimeout,
					opts.TLSConfig.Client,
				})
				connList = append(connList, abort)
			}
		case <-c.Done():
			yay = false
		}
	}
}

// Srv takes a Context and ConnOptions and begin listening for request to
// proxy.
// Srv obtains origin candidates through discovery service by key.  If the
// candidate list changes in discovery record, Srv will reject current
// connections and obtain new origin candidates.
// Review https://godoc.org/golang.org/x/net/context for understanding the
// control flow.
func Srv(c ctx.Context, opts *ConnOptions) error {
	if opts.Discovery == nil {
		panic("DiscOptions missing")
	}
	if candidates, err := obtain(opts.Discovery); err != nil {
		log.WithFields(log.Fields{"err": err}).Warning("Srv")
		opts.To = make([]string, 0)
	} else {
		opts.To = candidates
	}
	newConn, astp, err := acceptWorker(c, &config{
		opts.Net,
		opts.From,
		opts.TLSConfig.Server,
	})
	if err != nil {
		return err // something bad happend to Accepter
	}
	newNodes, wstp := watch(c, opts.Discovery) // spawn Watcher
	defer func() { _, _ = <-astp, <-wstp }()

	log.WithFields(log.Fields{"from": opts.From}).Debug("SRV start")
	if opts.Balance {
		balacnceSrv(newConn, newNodes, c, opts)
	} else {
		runSrv(newConn, newNodes, c, opts)
	}
	log.WithFields(log.Fields{"from": opts.From}).Debug("SRV stop")
	return ErrProxyEnd
}

// ClusterTo is a short hand to creating multiple connection endpoints.
// Proxy behavior behaves like To, but each source endpoint maps to one
// destination endpoint.
// Instead of managing connections yourself, this function helps you to handle
// connection as a group.
func ClusterTo(c ctx.Context, opts *ConnOptions) error {
	if len(opts.FromRange) != len(opts.To) {
		log.WithFields(log.Fields{"err": ErrClusterNodeMismatch}).Warning("ClusterTo")
	}
	var wg sync.WaitGroup
	for idx, from := range opts.FromRange {
		if idx+1 > len(opts.To) {
			log.WithFields(log.Fields{"err": ErrClusterNotEnoughNodes}).Warning("ClusterTo")
			continue
		}
		wg.Add(1)
		go func(from string, to []string) {
			// FIXME: need to report and err out
			To(c, &ConnOptions{
				Net:          opts.Net,
				From:         from,
				To:           to,
				TLSConfig:    opts.TLSConfig,
				ReadTimeout:  opts.ReadTimeout,
				WriteTimeout: opts.WriteTimeout,
			})
			wg.Done()
		}(from, []string{opts.To[idx]})
	}
	<-c.Done()
	wg.Wait()
	return ErrProxyEnd
}

// ClusterSrv is a short hand to creating multiple connection endpoints by
// service key.
// Proxy behavior behaves like Srv, but each source endpoint maps to one
// destination endpoint.
// Instead of managing connections yourself, this function helps you to handle
// connection as a group.
func ClusterSrv(c ctx.Context, opts *ConnOptions) error {
	if opts.Discovery == nil {
		panic("DiscOptions missing")
	}
	if candidates, err := obtain(opts.Discovery); err != nil {
		log.WithFields(log.Fields{"err": err}).Warning("ClusterSrv")
		opts.To = make([]string, 0)
	} else {
		opts.To = candidates
	}
	if len(opts.FromRange) != len(opts.To) {
		log.WithFields(log.Fields{"err": ErrClusterNodeMismatch}).Warning("ClusterSrv")
	}

	newNodes, wstp := watch(c, opts.Discovery) // spawn Watcher
	defer func() { <-wstp }()

	for yay := true; yay; {
		var wg sync.WaitGroup
		work, abort := ctx.WithCancel(c)
		for idx, from := range opts.FromRange {
			if idx+1 > len(opts.To) {
				log.WithFields(log.Fields{"err": ErrClusterNotEnoughNodes}).Warning("ClusterSrv")
				continue
			}
			wg.Add(1)
			go func(from string, to []string) {
				// FIXME: need to report and err out
				To(work, &ConnOptions{
					Net:          opts.Net,
					From:         from,
					To:           to,
					TLSConfig:    opts.TLSConfig,
					ReadTimeout:  opts.ReadTimeout,
					WriteTimeout: opts.WriteTimeout,
				})
				log.WithFields(log.Fields{"from": from, "to": to}).Debug("ClusterSrv")
				wg.Done()
			}(from, []string{opts.To[idx]})
		}
		for yelp := true; yelp; {
			select {
			case nodes := <-newNodes:
				if nodes != nil {
					opts.To = nodes
					abort()
					yelp = false
				}
			case <-c.Done():
				abort()
				yay, yelp = false, false
			}
		}
		wg.Wait()
	}

	return ErrProxyEnd
}
