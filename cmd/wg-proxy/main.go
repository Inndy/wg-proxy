package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"

	"go.inndy.tw/wg-proxy/wireguard/conf"

	"github.com/armon/go-socks5"
	wgConn "golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

var wg sync.WaitGroup

type stringList []string

func (i *stringList) String() string {
	return strings.Join(*i, ",")
}

func (i *stringList) Set(value string) error {
	*i = append(*i, value)
	return nil
}

type ResolverShim struct {
	net.Resolver
}

func (r *ResolverShim) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	ips, err := r.Resolver.LookupIP(ctx, "ip4", name)
	var ip net.IP
	if len(ips) > 0 {
		ip = ips[0]
	}
	if err != nil {
		log.Printf("Resolve %s -> failed: %s", name, err)
	} else {
		log.Printf("Resolve %s -> %s", name, ip)
	}
	return ctx, ip, err
}

func connect1(c1, c2 net.Conn) {
	io.Copy(c1, c2)
	if tcp1, ok := c1.(*net.TCPConn); ok {
		tcp1.CloseWrite()
	}
	if tcp2, ok := c2.(*net.TCPConn); ok {
		tcp2.CloseRead()
	}
}

func connect2(c1, c2 net.Conn) {
	ch := make(chan struct{})

	go func() {
		connect1(c1, c2)
		ch <- struct{}{}
	}()

	connect1(c2, c1)

	<-ch
}

func serveTcpProxy(wg *sync.WaitGroup, listener net.Listener, dial func(string, string) (net.Conn, error), forwardTo string) {
	defer wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Accept failed: %s", err)
			return
		}

		go func() {
			defer conn.Close()

			conn2, err := dial("tcp", forwardTo)
			if err != nil {
				log.Printf("Dial %s failed: %s", forwardTo, err)
				return
			}

			defer conn2.Close()
			connect2(conn, conn2)
		}()
	}
}

func main() {
	if len(os.Args) <= 1 {
		if args_raw, err := os.ReadFile("wg-proxy.conf"); err == nil {
			log.Printf("Reading args from wg-proxy.conf")
			os.Args = append(os.Args, strings.Fields(string(args_raw))...)
		}
	}
	var configFile, dnsServer string
	var localForwards, remoteForwards, socks5Proxies stringList

	flag.StringVar(&configFile, "config", "wg.conf", "WireGuard config file")
	flag.StringVar(&dnsServer, "dns", "8.8.8.8:53", "DNS server")
	flag.Var(&localForwards, "L", "Local port forward, like ssh -L")
	flag.Var(&remoteForwards, "R", "Remote port forward, like ssh -R")
	flag.Var(&socks5Proxies, "D", "Socks5 server listen on, like ssh -D")
	flag.Var(&socks5Proxies, "listen", "Socks5 server listen on")
	flag.Parse()

	if len(localForwards) == 0 && len(socks5Proxies) == 0 {
		socks5Proxies = append(socks5Proxies, "127.0.0.1:1080")
		log.Printf("Enable default socks5 listener: %s", socks5Proxies[0])
	}

	configBytes, err := os.ReadFile(configFile)
	if err != nil {
		log.Panicf("Can not read WireGuard config file: %s", err)
	}

	parsedConf, err := conf.FromWgQuick(string(configBytes), "wg")
	if err != nil {
		log.Panicf("Can not parse config file: %s", err)
	}

	ipc, err := parsedConf.IPC()
	if err != nil {
		log.Panicf("Can not generate WireGuard IPC config string: %s", err)
	}

	var tunAddrs []netip.Addr

	for _, addr := range parsedConf.Interface.Addresses {
		// XXX: should we add EVERY IP within the masked range?
		tunAddrs = append(tunAddrs, addr.Addr())
	}

	tun, tnet, err := netstack.CreateNetTUN(tunAddrs, []netip.Addr{netip.AddrFrom4([4]byte{1, 1, 1, 1})}, 1400)
	if err != nil {
		log.Panicf("netstack.CreateNetTUN: %s", err)
	}
	dev := device.NewDevice(tun, wgConn.NewDefaultBind(), device.NewLogger(device.LogLevelError, ""))

	if err := dev.IpcSet(ipc); err != nil {
		log.Panicf("dev.IpcSet: %s", err)
	}

	if err := dev.Up(); err != nil {
		log.Panicf("dev.Up: %s", err)
	}

	conf := &socks5.Config{
		Resolver: &ResolverShim{
			net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					return tnet.DialContext(ctx, network, dnsServer)
				},
			},
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "127.0.0.1:") {
				return nil, os.ErrInvalid
			}
			conn, err := tnet.DialContext(ctx, network, addr)
			if err != nil {
				log.Printf("Dial %s / %s -> failed: %s", network, addr, err)
			} else {
				log.Printf("Dial %s / %s -> success", network, addr)
			}
			return conn, err
		},
	}

	socks5server, err := socks5.New(conf)
	if err != nil {
		panic(err)
	}

	for _, listenOn := range socks5Proxies {
		listener, err := net.Listen("tcp", listenOn)
		if err != nil {
			log.Panicf("Can not listen on %s: %s", listenOn, err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			socks5server.Serve(listener)
		}()
	}

	for _, forward := range remoteForwards {
		args := strings.Split(forward, ":")

		var listenOn, forwardTo string

		switch len(args) {
		default:
			log.Panicf("Bad listener %s", forward)
		case 1:
			listenOn = ":" + args[0]
			forwardTo = "127.0.0.1:" + args[0]
		case 2:
			listenOn = ":" + args[0]
			forwardTo = "127.0.0.1:" + args[1]
		case 3:
			listenOn = ":" + args[0]
			forwardTo = args[1] + ":" + args[2]
		case 4:
			listenOn = args[0] + ":" + args[1]
			forwardTo = args[2] + ":" + args[3]
		}

		addr, err := net.ResolveTCPAddr("tcp", listenOn)
		if err != nil {
			log.Panicf("Can not resolve TCP address %s: %s", listenOn, err)
		}

		var listener net.Listener
		listener, err = tnet.ListenTCP(addr)
		if err != nil {
			log.Panicf("Can not listen on tcp:%s: %s", listenOn, err)
		}

		go serveTcpProxy(&wg, listener, net.Dial, forwardTo)
	}

	for _, forward := range localForwards {
		protocol := "tcp"
		if strings.HasPrefix(forward, "udp:") {
			forward = strings.TrimPrefix(forward, "udp:")
			protocol = "udp"
		}
		args := strings.Split(forward, ":")

		var listenOn, forwardTo string

		switch len(args) {
		default:
			log.Panicf("Bad listener %s", forward)
		case 3:
			listenOn = "127.0.0.1:" + args[0]
			forwardTo = args[1] + ":" + args[2]
		case 4:
			listenOn = args[0] + ":" + args[1]
			forwardTo = args[2] + ":" + args[3]
		}

		if protocol == "tcp" {
			wg.Add(1)
			listener, err := net.Listen("tcp", listenOn)
			if err != nil {
				log.Panicf("Can not listen on tcp:%s: %s", listenOn, err)
			}

			go serveTcpProxy(&wg, listener, tnet.Dial, forwardTo)
		} else if protocol == "udp" {
			wg.Add(1)

			udpForwardTo, err := net.ResolveUDPAddr("udp", forwardTo)
			if err != nil {
				log.Panicf("Can not resolve udp addr %s: %s", forwardTo, err)
			}

			listener, err := net.ListenPacket("udp", listenOn)
			if err != nil {
				log.Panicf("Can not listen on udp:%s: %s", listenOn, err)
			}

			var addrMap sync.Map

			go func() {
				defer wg.Done()
				for {
					var buf [65536]byte
					n, proxyClientAddr, err := listener.ReadFrom(buf[:])
					if err != nil {
						log.Printf("ReadFrom failed: %s", err)
						return
					}

					clientAddrKey := proxyClientAddr.(*net.UDPAddr).String()
					if val, ok := addrMap.Load(clientAddrKey); ok {
						val.(net.Conn).Write(buf[:n])
					} else {
						conn, err := tnet.DialUDP(nil, udpForwardTo)
						if err != nil {
							log.Printf("tnet.DialUDP failed: %s", err)
							continue
						}

						conn.Write(buf[:n])
						addrMap.Store(clientAddrKey, conn)

						wg.Add(1)
						go func() {
							defer wg.Done()
							var buf [65536]byte

							for {
								n, err := conn.Read(buf[:])
								if err != nil {
									log.Printf("udp proxy to %s conn.Read() from %s: %s", udpForwardTo, conn.LocalAddr(), err)
									break
								}

								listener.WriteTo(buf[:n], proxyClientAddr)
							}
						}()
					}
				}
			}()
		}
	}

	wg.Wait()
}
