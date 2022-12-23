package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"

	p2pd "github.com/libp2p/go-libp2p-daemon"
	config "github.com/libp2p/go-libp2p-daemon/config"
	ps "github.com/libp2p/go-libp2p-pubsub"
	connmgr "github.com/libp2p/go-libp2p/p2p/net/connmgr"
	noise "github.com/libp2p/go-libp2p/p2p/security/noise"
	tls "github.com/libp2p/go-libp2p/p2p/security/tls"
	multiaddr "github.com/multiformats/go-multiaddr"
	promhttp "github.com/prometheus/client_golang/prometheus/promhttp"

	_ "net/http/pprof"
)

func pprofHTTP(port int) {
	listen := func(p int) error {
		addr := fmt.Sprintf("localhost:%d", p)
		log.Printf("registering pprof debug http handler at: http://%s/debug/pprof/\n", addr)
		switch err := http.ListenAndServe(addr, nil); err {
		case nil:
			// all good, server is running and exited normally.
			return nil
		case http.ErrServerClosed:
			// all good, server was shut down.
			return nil
		default:
			// error, try another port
			log.Printf("error registering pprof debug http handler at: %s: %s\n", addr, err)
			return err
		}
	}

	if port > 0 {
		// we have a user-assigned port.
		_ = listen(port)
		return
	}

	// we don't have a user assigned port, try sequentially to bind between [6060-7080]
	for i := 6060; i <= 7080; i++ {
		if listen(i) == nil {
			return
		}
	}
}

func main() {
	maddrString := flag.String("listen", "/unix/tmp/p2pd.sock", "daemon control listen multiaddr")
	quiet := flag.Bool("q", false, "be quiet")
	id := flag.String("id", "", "peer identity; private key file")
	bootstrap := flag.Bool("b", false, "connects to bootstrap peers and bootstraps the dht if enabled")
	bootstrapPeers := flag.String("bootstrapPeers", "", "comma separated list of bootstrap peers; defaults to the IPFS DHT peers")
	dht := flag.Bool("dht", false, "Enables the DHT in full node mode")
	dhtClient := flag.Bool("dhtClient", false, "Enables the DHT in client mode")
	dhtServer := flag.Bool("dhtServer", false, "Enables the DHT in server mode (use 'dht' unless you actually need this)")
	connMgr := flag.Bool("connManager", false, "Enables the Connection Manager")
	connMgrLo := flag.Int("connLo", 256, "Connection Manager Low Water mark")
	connMgrHi := flag.Int("connHi", 512, "Connection Manager High Water mark")
	connMgrGrace := flag.Duration("connGrace", 120*time.Second, "Connection Manager grace period (in seconds)")
	flag.Bool("quic", true, "Enables the QUIC transport (deprecated, always enabled now)")
	natPortMap := flag.Bool("natPortMap", false, "Enables NAT port mapping")
	pubsub := flag.Bool("pubsub", false, "Enables pubsub")
	pubsubRouter := flag.String("pubsubRouter", "gossipsub", "Specifies the pubsub router implementation")
	pubsubSign := flag.Bool("pubsubSign", true, "Enables pubsub message signing")
	pubsubSignStrict := flag.Bool("pubsubSignStrict", true, "Enables or disables pubsub strict signature verification")
	gossipsubHeartbeatInterval := flag.Duration("gossipsubHeartbeatInterval", 0, "Specifies the gossipsub heartbeat interval")
	gossipsubHeartbeatInitialDelay := flag.Duration("gossipsubHeartbeatInitialDelay", 0, "Specifies the gossipsub initial heartbeat delay")

	relayEnabled := flag.Bool("relay", true, "Enables circuit relay")
	flag.Bool("relayActive", false, "Enables active mode for relay (deprecated, has no effect, use -relayService=1 instead)")
	flag.Bool("relayHop", false, "Enables hop for relay (deprecated, has no effect)")
	relayHopLimit := flag.Int("relayHopLimit", 0, "Sets the hop limit for hop relays (deprecated, has no effect)")
	autoRelay := flag.Bool("autoRelay", false, "Enables autorelay")
	relayDiscovery := flag.Bool("relayDiscovery", true, "Discover potential relays in background if -autoRelay=1")
	trustedRelaysRaw := flag.String("trustedRelays", "", "comma separated list of multiaddrs for static circuit relay peers; by default, use bootstrap peers as trusted relays")
	relayService := flag.Bool("relayService", true, "Configures this node to serve as a relay for others if -relayEnabled=1")
	relayMaxCircuits := flag.Int("relayMaxCircuits", 16, "maximum number of open relay connections for each peer if -relayService=1")
	relayMaxReservations := flag.Int("relayMaxReservations", 128, "maximum number of reserved relay slots for each peer if -relayService=1")
	relayBufferSize := flag.Int("relayBufferSize", 1 << 24, "size (in bytes) of the relayed connection buffers if -relayService=1")
	relayDataLimit := flag.Int64("relayDataLimit", 1 << 32, "maximum data bytes relayed (in each direction) before resetting connection if -relayService=1")
	relayTimeLimit := flag.Duration("relayTimeLimit", 30 * time.Minute, "maximum duration of a single active relayed connection if -relayService=1")

    autonat := flag.Bool("autonat", false, "Enables the AutoNAT service")
	hostAddrs := flag.String("hostAddrs", "", "comma separated list of multiaddrs the host should listen on")
	announceAddrs := flag.String("announceAddrs", "", "comma separated list of multiaddrs the host should announce to the network")
	noListen := flag.Bool("noListenAddrs", false, "sets the host to listen on no addresses")
	metricsAddr := flag.String("metricsAddr", "", "an address to bind the metrics handler to")
	configFilename := flag.String("f", "", "a file from which to read a json representation of the deamon config")
	configStdin := flag.Bool("i", false, "have the daemon read the json config from stdin")
	pprof := flag.Bool("pprof", false, "Enables the HTTP pprof handler, listening on the first port "+
		"available in the range [6060-7800], or on the user-provided port via -pprofPort")
	pprofPort := flag.Uint("pprofPort", 0, "Binds the HTTP pprof handler to a specific port; "+
		"has no effect unless the pprof option is enabled")
	useNoise := flag.Bool("noise", true, "Enables Noise channel security protocol")
	useTls := flag.Bool("tls", true, "Enables TLS1.3 channel security protocol")
	forceReachabilityPublic := flag.Bool("forceReachabilityPublic", false, "Set up ForceReachability as public for autonat")
	forceReachabilityPrivate := flag.Bool("forceReachabilityPrivate", false, "Set up ForceReachability as private for autonat")
	idleTimeout := flag.Duration("idleTimeout", 0,
		"Kills the daemon if no client opens a persistent connection in idleTimeout seconds."+
			" The zero value (default) disables this feature")
	persistentConnMaxMsgSize := flag.Int("persistentConnMaxMsgSize", 4*1024*1024,
		"Max size for persistent connection messages (bytes). Default: 4 MiB")

	flag.Parse()

	var c config.Config
	opts := []libp2p.Option{libp2p.UserAgent("p2pd/0.1")}

	if *configStdin {
		stdin := bufio.NewReader(os.Stdin)
		body, err := ioutil.ReadAll(stdin)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.Unmarshal(body, &c); err != nil {
			log.Fatal(err)
		}
	} else if *configFilename != "" {
		body, err := ioutil.ReadFile(*configFilename)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.Unmarshal(body, &c); err != nil {
			log.Fatal(err)
		}
	} else {
		c = config.NewDefaultConfig()
	}

	maddr, err := multiaddr.NewMultiaddr(*maddrString)
	if err != nil {
		log.Fatal(err)
	}
	c.ListenAddr = config.JSONMaddr{Multiaddr: maddr}

	if *id != "" {
		c.ID = *id
	}

	if *hostAddrs != "" {
		addrStrings := strings.Split(*hostAddrs, ",")
		ha := make([]multiaddr.Multiaddr, len(addrStrings))
		for i, s := range addrStrings {
			ma, err := multiaddr.NewMultiaddr(s)
			if err != nil {
				log.Fatal(err)
			}
			(ha)[i] = ma
		}
		c.HostAddresses = ha
	}

	if *announceAddrs != "" {
		addrStrings := strings.Split(*announceAddrs, ",")
		ha := make([]multiaddr.Multiaddr, len(addrStrings))
		for i, s := range addrStrings {
			ma, err := multiaddr.NewMultiaddr(s)
			if err != nil {
				log.Fatal(err)
			}
			(ha)[i] = ma
		}
		c.AnnounceAddresses = ha
	}

	if *connMgr {
		c.ConnectionManager.Enabled = true
		c.ConnectionManager.GracePeriod = *connMgrGrace
		c.ConnectionManager.HighWaterMark = *connMgrHi
		c.ConnectionManager.LowWaterMark = *connMgrLo
	}

	if *natPortMap {
		c.NatPortMap = true
	}

	if *relayEnabled {
		c.Relay.Enabled = true
		if *relayHopLimit > 0 {
			c.Relay.HopLimit = *relayHopLimit
		}
	}

	if *autoRelay {
		c.Relay.Auto = true
	}

	var trustedRelays []string
	if *trustedRelaysRaw != "" {
		trustedRelays = strings.Split(*trustedRelaysRaw, ",")
		if len(trustedRelays) > 0 && !*relayEnabled {
			panic("Found staticRelays but relays are not enabled, expected -relayEnabled=1")
		}
		if len(trustedRelays) > 0 && !*autoRelay {
			panic("Found staticRelays but autoRelay is not enabled, expected -autoRelay=1")
		}
	}

	if *autoRelay && !*relayDiscovery && len(trustedRelays) == 0 {
		panic("Daemon with autoRelay requires either -relayDiscovery=1 or -trustedRelays=$STATIC_RELAYS_HERE")
	}

	if *noListen {
		c.NoListen = true
	}

	if *autonat {
		c.AutoNat = true
	}

	if *pubsub {
		c.PubSub.Enabled = true
		c.PubSub.Router = *pubsubRouter
		c.PubSub.Sign = *pubsubSign
		c.PubSub.SignStrict = *pubsubSignStrict
		if *gossipsubHeartbeatInterval > 0 {
			c.PubSub.GossipSubHeartbeat.Interval = *gossipsubHeartbeatInterval
		}
		if *gossipsubHeartbeatInitialDelay > 0 {
			c.PubSub.GossipSubHeartbeat.InitialDelay = *gossipsubHeartbeatInitialDelay
		}
	}

	if *bootstrapPeers != "" {
		addrStrings := strings.Split(*bootstrapPeers, ",")
		bps := make([]multiaddr.Multiaddr, len(addrStrings))
		for i, s := range addrStrings {
			ma, err := multiaddr.NewMultiaddr(s)
			if err != nil {
				log.Fatal(err)
			}
			(bps)[i] = ma
		}
		c.Bootstrap.Peers = bps
	}

	if *bootstrap {
		c.Bootstrap.Enabled = true
	}

	if *quiet {
		c.Quiet = true
	}

	if *metricsAddr != "" {
		c.MetricsAddress = *metricsAddr
	}

	if *dht {
		c.DHT.Mode = config.DHTFullMode
	} else if *dhtClient {
		c.DHT.Mode = config.DHTClientMode
	} else if *dhtServer {
		c.DHT.Mode = config.DHTServerMode
	}

	if *pprof {
		c.PProf.Enabled = true
		if pprofPort != nil {
			c.PProf.Port = *pprofPort
		}
	}

	if useTls != nil {
		c.Security.TLS = *useTls
	}
	if useNoise != nil {
		c.Security.Noise = *useNoise
	}

	if err := c.Validate(); err != nil {
		log.Fatal(err)
	}

	if c.PProf.Enabled {
		// an invalid port number will fail within the function.
		go pprofHTTP(int(c.PProf.Port))
	}

	// collect opts
	if c.ID != "" {
		key, err := p2pd.ReadIdentity(c.ID)
		if err != nil {
			log.Fatal(err)
		}

		opts = append(opts, libp2p.Identity(key))
	}

	if len(c.HostAddresses) > 0 {
		opts = append(opts, libp2p.ListenAddrs(c.HostAddresses...))
	}

	if len(c.AnnounceAddresses) > 0 {
		opts = append(opts, libp2p.AddrsFactory(func([]multiaddr.Multiaddr) []multiaddr.Multiaddr {
			return c.AnnounceAddresses
		}))
	}

	if c.ConnectionManager.Enabled {
		cm, err := connmgr.NewConnManager(c.ConnectionManager.LowWaterMark,
			c.ConnectionManager.HighWaterMark,
			connmgr.WithGracePeriod(c.ConnectionManager.GracePeriod))
		if err != nil {
			log.Fatal(err)
		}
		opts = append(opts, libp2p.ConnectionManager(cm))
	}

	if c.NatPortMap {
		opts = append(opts, libp2p.NATPortMap())
	}

	if c.AutoNat {
		opts = append(opts, libp2p.EnableNATService())
        opts = append(opts, libp2p.EnableHolePunching())
	}

	if c.Relay.Enabled {
		opts = append(opts, libp2p.EnableRelay())

		if *relayService {
			opts = p2pd.ConfigureRelayService(opts, *relayMaxCircuits, *relayMaxReservations, *relayBufferSize, *relayDataLimit, *relayTimeLimit)
		}
	}

	if c.NoListen {
		opts = append(opts, libp2p.NoListenAddrs)
	}

	var securityOpts []libp2p.Option
	if c.Security.Noise {
		securityOpts = append(securityOpts, libp2p.Security(noise.ID, noise.New))
	}
	if c.Security.TLS {
		securityOpts = append(securityOpts, libp2p.Security(tls.ID, tls.New))
	}

	if len(securityOpts) == 0 {
		log.Fatal("at least one channel security protocol must be enabled")
	}
	opts = append(opts, securityOpts...)

	if *forceReachabilityPrivate && *forceReachabilityPublic {
		log.Fatal("forceReachability must be public or private, not both")
	} else if *forceReachabilityPrivate {
		opts = append(opts, libp2p.ForceReachabilityPrivate())
	} else if *forceReachabilityPublic {
		opts = append(opts, libp2p.ForceReachabilityPublic())
	}

	// start daemon
	d, err := p2pd.NewDaemon(
		context.Background(), &c.ListenAddr, c.DHT.Mode,
		*relayDiscovery, trustedRelays, *persistentConnMaxMsgSize,
		opts...)
	if err != nil {
		log.Fatal(err)
	}

	if *idleTimeout > 0 {
		d.KillOnTimeout(*idleTimeout)
	}

	if c.PubSub.Enabled {
		if c.PubSub.GossipSubHeartbeat.Interval > 0 {
			ps.GossipSubHeartbeatInterval = c.PubSub.GossipSubHeartbeat.Interval
		}
		if c.PubSub.GossipSubHeartbeat.InitialDelay > 0 {
			ps.GossipSubHeartbeatInitialDelay = c.PubSub.GossipSubHeartbeat.InitialDelay
		}

		err = d.EnablePubsub(c.PubSub.Router, c.PubSub.Sign, c.PubSub.SignStrict)
		if err != nil {
			log.Fatal(err)
		}
	}

	if len(c.Bootstrap.Peers) > 0 {
		p2pd.BootstrapPeers = c.Bootstrap.Peers
	}

	if c.Bootstrap.Enabled {
		err = d.Bootstrap()
		if err != nil {
			log.Fatal(err)
		}
	}

	if !c.Quiet {
		fmt.Printf("Control socket: %s\n", c.ListenAddr.String())
		fmt.Printf("Peer ID: %s\n", d.ID().Pretty())
		fmt.Printf("Peer Addrs:\n")
		for _, addr := range d.Addrs() {
			fmt.Printf("%s\n", addr.String())
		}
		if c.Bootstrap.Enabled && len(c.Bootstrap.Peers) > 0 {
			fmt.Printf("Bootstrap peers:\n")
			for _, p := range p2pd.BootstrapPeers {
				fmt.Printf("%s\n", p)
			}
		}
	}

	if c.MetricsAddress != "" {
		http.Handle("/metrics", promhttp.Handler())
		go func() { log.Println(http.ListenAndServe(c.MetricsAddress, nil)) }()
	}

	signal.Ignore(os.Interrupt)

	if err := d.Serve(); err != nil {
		log.Fatal(err)
	}
}
