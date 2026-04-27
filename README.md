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

# Terminal 2 — worker en mode heartbeat (sans modèle)
./griad worker --server ws://localhost:8080/ws/worker

# Terminal 3 — vérifier
curl http://localhost:8080/health
curl http://localhost:8080/api/workers
```

### Catalogue de modèles côté serveur

Le serveur tient un dossier de modèles GGUF (`--models-dir`, par défaut `.local/server-models/`). Un admin peut alimenter le catalogue depuis n'importe quelle URL directe (Ollama registry, releases GitHub, miroir HuggingFace authentifié, etc.) :

```bash
# Lister
./griad model list

# Télécharger un modèle dans le catalogue (avec progress en stream)
./griad model pull qwen2.5-3b https://registry.ollama.ai/v2/library/qwen2.5/blobs/sha256:5ee4f07cdb9beadbbb293e85803c569b01bd37ed059d2715faa7bb405f31caa6
```

Les modèles sont aussi servis sur `GET /api/models/{name}` (avec support `Range` pour resume), ce qui permettra aux workers de les fetcher automatiquement à terme.

### Worker avec un vrai modèle

Le worker supervise `llama-server` (binaire de [llama.cpp](https://github.com/ggml-org/llama.cpp/releases)). Deux façons de lui donner un modèle :

**Option A — depuis le catalogue serveur** (recommandé) :

```bash
./griad worker \
  --server http://localhost:8080 \
  --llama-server /chemin/vers/llama-server[.exe] \
  --catalog-model qwen2.5-3b
```

Le worker télécharge le modèle depuis l'orchestrateur (`/api/models/{name}`), le cache localement dans `.local/worker-models/`, puis lance `llama-server` dessus. Au prochain démarrage, le cache évite de re-télécharger.

**Option B — fichier local** (utile en dev) :

```bash
./griad worker \
  --server http://localhost:8080 \
  --llama-server /chemin/vers/llama-server[.exe] \
  --model /chemin/vers/modele.gguf
```

Dans les deux cas, le worker spawne `llama-server` sur un port libre local (127.0.0.1), attend que `/health` réponde OK, puis se déclare au serveur avec son modèle chargé. Si llama-server crashe, le worker se déconnecte.

> Le flag `--server` accepte aussi l'ancienne forme `ws://host:8080/ws/worker` pour compat.

### Chat distribué

Le TUI :

```bash
./griad chat --server http://localhost:8080
```

Au démarrage le TUI fetch `/api/workers`, affiche le modèle dispo dans la barre de statut, et te laisse discuter. `Entrée` envoie, `Maj+Entrée` saute une ligne, `Esc` annule la génération en cours, `Ctrl+C` quitte. Les tokens streamés du worker apparaissent en temps réel.

Ou directement en HTTP (utile pour scripter ou intégrer un autre client OpenAI-compatible) :

```bash
curl -N -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"smollm2","messages":[{"role":"user","content":"Hello"}],"max_tokens":40}'
```

L'orchestrateur pick un worker, lui envoie la requête via WS, le worker l'exécute contre son `llama-server` local et streame les tokens en SSE jusqu'au client. Si le client se déconnecte, l'annulation est propagée jusqu'à llama-server.

## Roadmap MVP

- [x] Squelette repo + binaire multi-commandes
- [x] Registre des workers + heartbeat WS
- [x] Worker : démarrage de `llama-server` en sous-processus
- [x] Endpoint chat OpenAI-compatible côté serveur, routé vers un worker capable (avec streaming SSE et propagation des annulations)
- [x] TUI chat (Bubble Tea)
- [x] Catalogue de modèles côté serveur (download GGUF + API list/pull)
- [x] Worker : download depuis le catalogue serveur via `--catalog-model NAME`
- [ ] Idle detection Windows (`GetLastInputInfo`)
- [ ] Auto-download de llama.cpp côté worker (vers l'autonomie totale)

Plus tard : auth, chiffrement, vérification des outputs, scheduling load-aware, dashboard.

## Licence

MIT — voir [LICENSE](LICENSE).
