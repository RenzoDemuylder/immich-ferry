#!/bin/sh
# Run immich-ferry as a plain container. Secrets and settings come from sync.env (see sync.env.example).
set -eu
IMAGE="${IMAGE:-immich-ferry:latest}"
ENV_FILE="${ENV_FILE:-sync.env}"

# /tmp is a disk-backed named volume. A RAM-backed alternative (opt-in, counts against memory; also set
# --memory well above the size and lower SYNC_TMP_BUDGET):  --tmpfs /tmp:rw,size=4g,mode=1777
docker run -d --name immich-ferry \
  --restart unless-stopped \
  --stop-timeout 60 \
  --read-only \
  -v immich-ferry-tmp:/tmp \
  -e SYNC_TMP_DIR=/tmp -e SYNC_TMP_BUDGET=17179869184 \
  -v immich-ferry-data:/data \
  --env-file "$ENV_FILE" \
  -p 127.0.0.1:8080:8080 \
  "$IMAGE"

echo "logs:    docker logs -f immich-ferry"
echo "status:  curl -s localhost:8080/status"
