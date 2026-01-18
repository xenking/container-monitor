# Docker Monitor (Telegram)

A lightweight, self-hosted tool written in Go to monitor Docker containers. It listens to the Docker socket and sends a notification to Telegram whenever a container dies or crashes.

## Features

- Real-time alerts: Instant notifications via Telegram.
- Docker Integration: Listens directly to Docker events (die action).
- Filtering: Whitelist or blacklist specific containers by name.
- Zero Dependencies: Connects via Unix socket, no extra agents required.
- Clean Logs: Option to ignore manual stops or graceful exits (exit code 0).

## Quick Start

### 1. Docker Compose (Recommended)

Create a docker-compose.yml file:

```yaml
services:
  monitor:
    image: ghcr.io/xenkings/docker-monitor:latest
    container_name: docker-monitor
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    env_file:
      - .env

```

### 2. Configuration (.env)

Create a .env file next to your compose file.

**Minimal configuration:**

```dotenv
TG_TOKEN=123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11
TG_CHAT_ID=123456789
SERVER_NAME=Production-Server

```

**Advanced configuration (with filters):**

```dotenv
# Required
TG_TOKEN=your_bot_token
TG_CHAT_ID=your_chat_id

# Optional: Server Identifier (useful if you have multiple servers)
SERVER_NAME=My-Home-Server

# Optional: Filter Logic
# Only listen to these specific containers (comma separated)
INCLUDE_CONTAINERS=postgres,nginx-proxy

# Ignore these specific containers (comma separated)
# Note: INCLUDE takes precedence. If INCLUDE is set, EXCLUDE is ignored.
EXCLUDE_CONTAINERS=test-runner,temp-worker

# Optional: Behavior
# Set to false if you want alerts even when you stop a container manually (exit 0)
IGNORE_EXIT_ZERO=true

# Optional: Timeouts
RECONNECT_DELAY=5s
REQUEST_TIMEOUT=10s

```

### 3. Run

```bash
docker compose up -d

```

## Configuration Reference

| Variable | Description | Default |
| --- | --- | --- |
| TG_TOKEN | Required. Telegram Bot Token from @BotFather. | - |
| TG_CHAT_ID | Required. User or Group ID to receive alerts. | - |
| SERVER_NAME | Name of the host displayed in the alert. | MyServer |
| INCLUDE_CONTAINERS | Comma-separated list of container names to monitor. If set, all others are ignored. | [] |
| EXCLUDE_CONTAINERS | Comma-separated list of container names to ignore. | [] |
| IGNORE_EXIT_ZERO | If true, ignores containers that exit successfully (code 0). | true |
| RECONNECT_DELAY | Time to wait before reconnecting to Docker daemon. | 5s |
| REQUEST_TIMEOUT | Timeout for Telegram API requests. | 10s |

## Build Locally

If you want to build the image yourself:

```bash
docker build -t docker-monitor .

```
