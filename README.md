# ttunnel

`ttunnel` is a small reverse TCP tunnel for exposing a TCP service from an
agent machine through a publicly reachable server.

It is intended for controlled environments such as a private network, VPN, or
SSH tunnel.

## Architecture

```text
   +-----------------------------------+
   | Laptop                            |
   |                                   |
   |  +----------------+               |
   |  | local TCP      |               |
   |  | service        |               |
   |  +-------+--------+               |
   |          ^                        |
   |          | localhost:<target>     |
   |          v                        |   outbound control connection
   |  +----------------+               |   and tunneled TCP streams
   |  | ttunnel agent  +---------------+----------------------+
   |  +----------------+               |                      |
   +-----------------------------------+                      v
                                                    +------------------+
                                                    | Cloud VM         |
                                                    |                  |
                                                    | +--------------+ |
                                                    | | ttunnel      | |
                                                    | | server       | |
                                                    | | public IP    | |
                                                    | +------+-------+ |
                                                    |        ^         |
                                                    |        | public:<target>
                                                    +--------+---------+
                                                             ^
                                                             |
                                                    +------------------+
                                                    | public clients   |
                                                    | connect here     |
                                                    +------------------+

Public client traffic enters the cloud VM, crosses the existing agent
connection, and reaches the local service running on the laptop.
```

## Install

```sh
go install github.com/marcoandredinis/ttunnel@v1.0.1
```

## Usage

Start the server:

```sh
ttunnel server
```

Start the agent with the token printed by the server:

```sh
TTUNNEL_TOKEN=<token> ttunnel agent --server <server-host>:8001 --target 3000
```

`--target` is both the local port on the agent and the public port opened on
the server.

## Security

The only built-in security layer is a random bearer token checked during the
agent handshake.

Traffic is still sent over plain TCP. There is no TLS, transport encryption,
server identity verification, or message integrity protection. Do not use this
directly on an untrusted network or expose it directly to the public Internet.

## Maintenance

This project is considered finished software. It will only be updated when a
critical bug is discovered.
