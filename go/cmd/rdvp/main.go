package main

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	mrand "math/rand"
	"net"
	"os"
	"strconv"
	"strings"

	"berty.tech/berty/v2/go/pkg/errcode"
	libp2p "github.com/libp2p/go-libp2p"
	libp2p_cicuit "github.com/libp2p/go-libp2p-circuit"
	libp2p_ci "github.com/libp2p/go-libp2p-core/crypto" // nolint:staticcheck
	libp2p_host "github.com/libp2p/go-libp2p-core/host"
	libp2p_peer "github.com/libp2p/go-libp2p-core/peer"
	libp2p_quic "github.com/libp2p/go-libp2p-quic-transport"
	libp2p_rp "github.com/libp2p/go-libp2p-rendezvous"
	libp2p_rpdb "github.com/libp2p/go-libp2p-rendezvous/db/sqlite"

	ipfs_log "github.com/ipfs/go-log"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/oklog/run"
	"github.com/peterbourgon/ff"
	"github.com/peterbourgon/ff/ffcli"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"moul.io/srand"
)

func main() {
	log.SetFlags(0)

	var (
		process run.Group

		logger          *zap.Logger
		globalFlags     = flag.NewFlagSet("rdvp", flag.ExitOnError)
		globalDebug     = globalFlags.Bool("debug", false, "debug mode")
		globalLogToFile = globalFlags.String("logfile", "", "if specified, will log everything in JSON into a file and nothing on stderr")

		serveFlags          = flag.NewFlagSet("serve", flag.ExitOnError)
		serveFlagsURN       = serveFlags.String("db", ":memory:", "rdvp sqlite URN")
		serveFlagsListeners = serveFlags.String("l", "/ip4/0.0.0.0/tcp/4040,/ip4/0.0.0.0/udp/4141/quic", "lists of listeners of (m)addrs separate by a comma")
		serveFlagsPK        = serveFlags.String("pk", "", "private key (generated by `rdvp genkey`)")
	)

	globalPreRun := func() (err error) {
		mrand.Seed(srand.Secure())
		logger, err = newLogger(*globalDebug, *globalLogToFile)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// handle close signal
	execute, interrupt := run.SignalHandler(ctx, os.Interrupt)
	process.Add(execute, interrupt)

	serve := &ffcli.Command{
		Name:    "serve",
		Usage:   "serve -l <maddrs> -pk <private_key> -db <file>",
		FlagSet: serveFlags,
		Options: []ff.Option{ff.WithEnvVarPrefix("RDVP")},
		Exec: func(args []string) error {
			if err := globalPreRun(); err != nil {
				return errcode.TODO.Wrap(err)
			}

			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			laddrs := strings.Split(*serveFlagsListeners, ",")
			listeners, err := parseAddrs(laddrs...)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			// load existing or generate new identity
			var priv libp2p_ci.PrivKey
			if *serveFlagsPK != "" {
				kBytes, err := base64.StdEncoding.DecodeString(*serveFlagsPK)
				if err != nil {
					return errcode.TODO.Wrap(err)
				}
				priv, err = libp2p_ci.UnmarshalPrivateKey(kBytes)
				if err != nil {
					return errcode.TODO.Wrap(err)
				}
			} else {
				priv, _, err = libp2p_ci.GenerateKeyPairWithReader(libp2p_ci.RSA, 2048, crand.Reader) // nolint:staticcheck
				if err != nil {
					return errcode.TODO.Wrap(err)
				}
			}

			// init p2p host
			host, err := libp2p.New(ctx,
				// default tpt + quic
				libp2p.DefaultTransports,
				libp2p.Transport(libp2p_quic.NewTransport),

				// Nat & Relay service
				libp2p.EnableNATService(),
				libp2p.DefaultStaticRelays(),
				libp2p.EnableRelay(libp2p_cicuit.OptHop),

				// swarm listeners
				libp2p.ListenAddrs(listeners...),

				// identity
				libp2p.Identity(priv),
			)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}
			defer host.Close()
			logHostInfo(logger, host)

			db, err := libp2p_rpdb.OpenDB(ctx, *serveFlagsURN)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			defer db.Close()

			// start service
			_ = libp2p_rp.NewRendezvousService(host, db)

			<-ctx.Done()
			if err = ctx.Err(); err != nil {
				return errcode.TODO.Wrap(err)
			}
			return nil
		},
	}

	genkey := &ffcli.Command{
		Name: "genkey",
		Exec: func(args []string) error {
			priv, _, err := libp2p_ci.GenerateKeyPairWithReader(libp2p_ci.RSA, 2048, crand.Reader) // nolint:staticcheck
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			kBytes, err := libp2p_ci.MarshalPrivateKey(priv)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			fmt.Println(base64.StdEncoding.EncodeToString(kBytes))
			return nil
		},
	}

	root := &ffcli.Command{
		Usage:       "rdvp [global flags] <subcommand> [flags] [args...]",
		FlagSet:     globalFlags,
		Options:     []ff.Option{ff.WithEnvVarPrefix("RDVP")},
		Subcommands: []*ffcli.Command{serve, genkey},
		Exec: func([]string) error {
			globalFlags.Usage()
			return flag.ErrHelp
		},
	}

	// add root command to process
	process.Add(func() error {
		return root.Run(os.Args[1:])
	}, func(error) {
		cancel()
	})

	// run process
	if err := process.Run(); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}

// helpers

func logHostInfo(l *zap.Logger, host libp2p_host.Host) {
	// print peer addrs
	fields := []zapcore.Field{
		zap.String("host ID (local)", host.ID().String()),
	}

	addrs := host.Addrs()
	pi := libp2p_peer.AddrInfo{
		ID:    host.ID(),
		Addrs: addrs,
	}
	if maddrs, err := libp2p_peer.AddrInfoToP2pAddrs(&pi); err == nil {
		for _, maddr := range maddrs {
			fields = append(fields, zap.Stringer("maddr", maddr))
		}
	}

	l.Info("host started", fields...)
}

func parseAddrs(addrs ...string) (maddrs []ma.Multiaddr, err error) {
	maddrs = make([]ma.Multiaddr, len(addrs))
	for i, addr := range addrs {
		maddrs[i], err = ma.NewMultiaddr(addr)

		if err != nil {
			// try to get a tcp multiaddr from host:port
			host, port, serr := net.SplitHostPort(addr)
			if serr != nil {
				return
			}

			if host == "" {
				host = "127.0.0.1"
			}

			addr = fmt.Sprintf("/ip4/%s/tcp/%s/", host, port)
			maddrs[i], err = ma.NewMultiaddr(addr)
		}
	}

	return
}

func parseBoolFromEnv(key string) (b bool) {
	b, _ = strconv.ParseBool(os.Getenv(key))
	return
}

func newLogger(debug bool, logfile string) (*zap.Logger, error) {
	bertyDebug := parseBoolFromEnv("BERTY_DEBUG") || debug
	libp2pDebug := parseBoolFromEnv("LIBP2P_DEBUG")
	// @NOTE(gfanton): since orbitdb use `zap.L()`, this will only
	// replace zap global logger with our logger
	orbitdbDebug := parseBoolFromEnv("ORBITDB_DEBUG")

	isDebugEnabled := bertyDebug || orbitdbDebug || libp2pDebug

	// setup zap config
	var config zap.Config
	if logfile != "" {
		config = zap.NewProductionConfig()
		config.OutputPaths = []string{logfile}
	} else {
		config = zap.NewDevelopmentConfig()
		config.DisableStacktrace = true
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	if isDebugEnabled {
		config.Level.SetLevel(zap.DebugLevel)
	} else {
		config.Level.SetLevel(zap.InfoLevel)
	}

	logger, err := config.Build()
	if err != nil {
		return nil, errcode.TODO.Wrap(err)
	}

	if libp2pDebug {
		ipfs_log.SetDebugLogging()
	}

	if orbitdbDebug {
		zap.ReplaceGlobals(logger)
	}

	return logger, nil
}
