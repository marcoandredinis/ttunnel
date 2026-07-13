# ttunnel

`ttunnel` exposes a local TCP port through a reachable server. Use it only on a
trusted private network, VPN, or SSH tunnel.

## Usage

```text
ttunnel server
TTUNNEL_TOKEN=<token> ttunnel agent <server-host> [--target <port>]
```

`<server-host>` is the public host name or IP address without a port. The agent
always connects to its control port, `8001`. `--target` defaults to `443` and
is used for both the local service and the server's public listener.

Start the server on the reachable host:

```sh
ttunnel server
```

On the laptop running the local service, use the token printed by the server:

```sh
TTUNNEL_TOKEN=<token> ttunnel agent <server-host> --target 3000
```

## Architecture

```text
Laptop / private network                                                Public cloud

+---------------------------+     long running control connection      +---------------------------+
|   ttunnel agent           |=========================================>|   ttunnel server          |
|                           |                                          | binds control :8001       |
|             +-------------|<---------- public tcp connections--------|<-----------+              |
|             |             |=========================================>|            |              |
+-------------+-------------+                                          +------------+--------------+
              |                                                                     |
              v                                                                     |
 localhost:<target>                                                                 |
   local service                                                              public tcp connections :<target>
```

1. Run the agent on the laptop that can reach the local service. It opens the
   large, persistent control connection to the server in the public cloud on
   `server:8001`, then sends its bearer token and target port.
2. After authentication, the server binds `:<target>` and confirms the tunnel.
3. Each client connection to `server:<target>` becomes one yamux stream over
   the existing agent connection. The agent proxies that stream to
   `localhost:<target>`.

The target port is shared locally and publicly. The server closes its public
listener when the agent session ends.

## Install

```sh
go install github.com/marcoandredinis/ttunnel@v1.0.6
```

## Security

Authentication is a random bearer token checked during the agent handshake.
Traffic is plain TCP: there is no TLS, encryption, server identity verification,
or integrity protection. Do not expose it directly to the public Internet.

## Maintenance

This project is considered finished software. It will only be updated when a
critical bug is discovered.
