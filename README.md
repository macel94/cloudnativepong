# 🏓 Cloud Native Pong

A minimalist, horizontally-scalable PONG game running on Kubernetes. Each game room is a dedicated pod — demonstrating the power of cloud-native architecture with near-zero overhead.

## ✨ Features

- **Zero-auth** — Just pick a name and play. No accounts, no passwords.
- **1 room = 1 pod** — Every game room runs in its own isolated pod. Scale infinitely.
- **Blazing fast** — Go binary (~6 MB), `scratch` container image, sub-millisecond WebSocket messages.
- **Embedded SQLite** — Pure Go, CGO-free, no external database needed.
- **Self-cleaning** — Rooms auto-terminate when the game ends. No orphaned pods.

## 🏗 Architecture

```
                   ┌──────────────┐
                   │   Lobby Pod  │  (static, 1 replica)
                   │  :8080       │
                   └──────┬───────┘
                          │ creates room pods via K8s API
          ┌───────────────┼───────────────┐
          ▼               ▼               ▼
   ┌────────────┐  ┌────────────┐  ┌────────────┐
   │  Room Pod  │  │  Room Pod  │  │  Room Pod  │  (dynamic, 1 per room)
   │  :8080     │  │  :8080     │  │  :8080     │
   └────────────┘  └────────────┘  └────────────┘
```

1. Players open the **lobby** → see active rooms → create or join a room.
2. The lobby spawns a **room pod** via the Kubernetes API.
3. Players connect directly to the room pod via **WebSocket**.
4. Two players play PONG in real-time. The room pod terminates after the game ends.

## 🚀 Quick Start

### Local Development (no Kubernetes)

```bash
# Run everything in a single process
go run . --mode=local

# Open http://localhost:8080
```

### On Kubernetes

```bash
# Build and push the image
docker build -t cloudnativepong:latest .
# (push to your registry)

# Deploy
kubectl apply -f k8s/
```

## 🧪 Testing

```bash
# Install Playwright
npm install

# Run E2E tests
npx playwright test
```

## 📁 Project Structure

```
.
├── main.go              # Entry point, routing, CLI flags
├── lobby/               # Lobby server (room management, K8s integration)
├── game/                # PONG game engine (server-side state machine)
├── db/                  # SQLite database layer
├── static/              # Frontend (HTML, CSS, vanilla JS)
├── k8s/                 # Kubernetes manifests
├── tests/               # Playwright E2E tests
├── Dockerfile           # Multi-stage, scratch-based
└── README.md
```

## 🔧 Tech Stack

| Layer          | Choice                        | Why                                      |
| -------------- | ----------------------------- | ---------------------------------------- |
| Language       | Go 1.22+                      | Single binary, no runtime, tiny images   |
| Database       | SQLite (modernc.org/sqlite)   | Pure Go, embedded, zero ops              |
| WebSocket      | gorilla/websocket             | Battle-tested, minimal API               |
| Container      | `scratch`                     | Smallest possible image (~6 MB)          |
| Frontend       | Vanilla JS + Canvas           | No framework, instant load               |
| E2E Testing    | Playwright + TypeScript       | Reliable, cross-browser                  |

## 🎮 How to Play

1. Open the lobby page.
2. Enter a display name.
3. Click **New Room** (and share the link) or **Join** an existing room.
4. Use **W/S** (Player 1) or **↑/↓** (Player 2) to move your paddle.
5. First to 7 points wins!

---

**Made to demonstrate cloud-native scalability with minimal complexity.**