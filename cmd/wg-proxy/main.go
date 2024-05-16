package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"

	"go.inndy.tw/wg-proxy/wireguard/conf"

	"github.com/armon/go-socks5"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

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

func main() {
	var configFile, dnsServer, listenOn string

	flag.StringVar(&configFile, "config", "wg.conf", "WireGuard config file")
	flag.StringVar(&dnsServer, "dns", "8.8.8.8:53", "DNS server")
	flag.StringVar(&listenOn, "listen", "127.0.0.1:1080", "Socks5 server listen on")
	flag.Parse()

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

	tun, tnet, err := netstack.CreateNetTUN(tunAddrs, []netip.Addr{netip.AddrFrom4([4]byte{10, 0, 0, 2})}, 1400)
	if err != nil {
		log.Panicf("netstack.CreateNetTUN: %s", err)
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, ""))

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
	server, err := socks5.New(conf)
	if err != nil {
		panic(err)
	}

	if err := server.ListenAndServe("tcp", listenOn); err != nil {
		panic(err)
	}
}
