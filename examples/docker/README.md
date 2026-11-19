# Plain Docker examples

Files: `docker-run.sh`, `compose.yaml`, `sync.env.example`, `config.example.yaml`.

1. Build the image (`make image` in the repo root) or use your registry image.
2. `cp sync.env.example sync.env`, fill in URLs, API keys and albums, `chmod 600 sync.env`.
3. `./docker-run.sh` or `docker compose up -d`.

Notes:

- Always keep `/data` on a volume: it holds the sync state (removal tracking, metadata baselines).
  Without it, a container re-create forgets what was removed (nothing is deleted, but removed items may be re-added).
- Use `--stop-timeout 60` / `stop_grace_period: 60s` so a stop mid-transfer can save state cleanly.
- `/tmp` holds in-flight downloads (videos are streamed to disk). With `--read-only`, the examples mount a disk-backed named
  volume (`immich-ferry-tmp`) there; set `SYNC_TMP_BUDGET` to at most ~80% of its free space. A tmpfs is RAM-backed and counts against
  container memory: opt in only with a memory limit well above its size (commented in `compose.yaml`).
- Admin commands: `docker exec immich-ferry /immich-ferry approve-removals --pair <name>`,
  `docker exec immich-ferry /immich-ferry state show --pair <name>`,
  `docker exec immich-ferry /immich-ferry state reset --pair <name> --yes`.
- Multiple pairs: mount `config.example.yaml` (adapted) and set `SYNC_CONFIG_FILE`. Put API keys in files and reference them with
  `apiKeyFile`, or keep `IMMICH_{A,B}_API_KEY` in the env file.
- Custom CA: mount the bundle and set `SSL_CERT_FILE`.
