# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

wg-proxy converts WireGuard VPN to a SOCKS5 proxy. It creates a userspace WireGuard tunnel using netstack and exposes it as a local SOCKS5 proxy or port forwarding service.

## Build & Run

```bash
# Build
make wg-proxy

# Build Alternative
go build ./cmd/wg-proxy

# Run with default config (wg.conf) and default SOCKS5 listener (127.0.0.1:1080)
./wg-proxy

# Specify config file and options
./wg-proxy -config path/to/config.conf -listen 127.0.0.1:8080

# Read arguments from wg-proxy.conf if no args provided
# (automatically tries to load wg-proxy.conf when no CLI args given)
```

## Command-Line Arguments

The tool supports SSH-style arguments:

- `-config <file>` - wg-quick style config file (default: wg.conf)
- `-dns <server>` - DNS server (default: 8.8.8.8:53)
- `-D <addr>` or `-listen <addr>` - SOCKS5 proxy listener (like ssh -D)
- `-L <forward>` - Local port forward (like ssh -L)
  - Format: `[local_ip:]local_port:remote_host:remote_port`
  - Example: `8080:example.com:80` or `127.0.0.1:8080:example.com:80`
  - UDP support: prefix with `udp:` (e.g., `udp:5353:dns.example.com:53`)
- `-R <forward>` - Remote port forward (like ssh -R)
  - Format: `[listen_ip:]listen_port[:forward_host]:forward_port`
  - Listens on WireGuard interface, forwards to local machine

## Architecture

### Core Components

1. **WireGuard Tunnel Setup** (main.go:138-157)
   - Uses `netstack.CreateNetTUN` to create userspace TUN device
   - Configures WireGuard device with IPC protocol
   - Brings up the tunnel interface

2. **SOCKS5 Proxy** (main.go:159-198)
   - Custom resolver that uses WireGuard tunnel for DNS
   - All connections dialed through WireGuard tunnel network stack
   - Blocks connections to 127.0.0.1 to prevent loops

3. **Port Forwarding**
   - **Local (-L)**: Listens locally, forwards through WireGuard tunnel
   - **Remote (-R)**: Listens on WireGuard interface, forwards to local network
   - TCP forwarding uses bidirectional copy with proper connection shutdown
   - UDP forwarding maintains address mapping for stateful packet relay

### WireGuard Config Parser (wireguard/conf/)

- `parser.go` - Parses WireGuard configuration files (wg-quick format)
- `config.go` - Config data structures and IPC generation
- Uses WireGuard official types and follows WireGuard LLC's implementation

### Key Implementation Details

- **Connection Bridging** (main.go:53-74): Uses goroutines with proper TCP half-close
- **UDP Forwarding** (main.go:278-320): Maintains client address map with per-client relay goroutines
- **Fallback Config**: Reads from `wg-proxy.conf` if no CLI arguments provided (main.go:101-106)
- **DNS Resolution**: All DNS queries go through WireGuard tunnel using custom resolver

## Testing

No test files present in the repository. Manual testing required with a valid WireGuard configuration.
