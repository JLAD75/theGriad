# theGriad

Une grille intelligente d'inférence distribuée. Installe le client sur ta machine — quand elle est inactive, elle contribue de la capacité d'inférence à la grille. Quand tu veux, tu peux toi aussi utiliser la grille pour discuter avec des LLM hébergés par les autres volontaires.

> Status : **alpha** — squelette. Le serveur et le worker se parlent en WebSocket, c'est tout pour l'instant. L'inférence et le chat TUI arrivent.

## Architecture

```
┌──────────────────┐        ┌──────────────────────┐        ┌──────────────────┐
│  griad chat      │        │   griad server       │        │  griad worker    │
│  (TUI utilisateur)│ ─SSE─> │  - catalogue modèles │ <─WS─> │  llama-server    │
└──────────────────┘        │  - registre workers  │        │  + idle detect   │
                            │  - scheduler         │        └──────────────────┘
                            └──────────────────────┘
```

Trois rôles, **un seul binaire** `griad` :
- `griad server` — l'orchestrateur. Tient un catalogue de modèles GGUF, un registre des workers connectés, route les requêtes de chat.
- `griad worker` — le contributeur. Se connecte à un serveur, télécharge les modèles assignés, lance `llama-server` en local et exécute les requêtes d'inférence quand la machine est inactive.
- `griad chat` — le client utilisateur (TUI). Se connecte à un serveur, choisit un modèle disponible, discute.

## Stack

- **Go** : un binaire statique unique, zéro install pour l'utilisateur final, cross-platform.
- **llama.cpp** côté worker : runtime d'inférence (binaires précompilés CPU/CUDA/Vulkan/Metal, format GGUF).
- **WebSocket** pour le contrôle worker ↔ serveur, **HTTP+SSE** pour streamer les tokens vers le TUI.

## Quickstart (dev)

```bash
go mod tidy
go build -o griad ./cmd/griad

# Terminal 1 — serveur
./griad server --addr :8080

# Terminal 2 — worker
./griad worker --server ws://localhost:8080/ws/worker

# Terminal 3 — vérifier
curl http://localhost:8080/health
curl http://localhost:8080/api/workers
```

## Roadmap MVP

- [x] Squelette repo + binaire multi-commandes
- [x] Registre des workers + heartbeat WS
- [ ] Catalogue de modèles côté serveur (download GGUF)
- [ ] Worker : démarrage de `llama-server` en sous-processus
- [ ] Endpoint chat OpenAI-compatible côté serveur, routé vers un worker capable
- [ ] TUI chat (Bubble Tea)
- [ ] Idle detection Windows (`GetLastInputInfo`)

Plus tard : auth, chiffrement, vérification des outputs, scheduling load-aware, dashboard.

## Licence

MIT — voir [LICENSE](LICENSE).
