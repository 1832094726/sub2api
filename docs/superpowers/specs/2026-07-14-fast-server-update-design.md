# Sub2API Fast Server Update Design

## Goal

Provide one local command that updates the Sub2API deployment on the Aliyun server without rebuilding the memory-intensive frontend there. Preserve the current database, Redis, environment, Cloudflare Tunnel, and rollback capability.

## Command

```bash
./deploy/fast-update.sh
```

Optional flags:

- `--skip-tests`: skip focused local tests when the caller has already run them.
- `--backend-only`: reuse the last locally generated frontend bundle.
- `--tag <tag>`: override the generated immutable image tag.

## Pipeline

1. Validate local tools, SSH access, repository state, and required paths.
2. Run focused Go unit tests for the changed channel-monitor behavior.
3. Build the frontend locally so `backend/internal/web/dist` is ready for Go embedding.
4. Incrementally synchronize backend source and deployment Dockerfile to `/opt/sub2api-src` with `rsync`.
5. On the server, tag the currently running image as the local runtime base.
6. Build only the Go backend with a persistent Docker volume for the Go module/build cache.
7. Create an immutable image tag derived from timestamp and Git commit.
8. Update only the `sub2api` Compose service through an override file and recreate that container.
9. Verify container health, local health endpoint, Cloudflare domain homepage, and unauthenticated `/v1/responses` returning the expected `401`.
10. On failure, restore the previous image in the override file and recreate the container.

## Network And Cache

- Local frontend package installation uses the existing pnpm store.
- The server build uses `GOPROXY=https://goproxy.cn,direct` and `GOSUMDB=sum.golang.google.cn`.
- The backend builder reuses persistent Go module and compilation caches across deployments.
- Docker image construction uses the locally tagged running image as its final runtime base and never queries Docker Hub for that base.
- Mihomo remains available for future explicit proxy use, but the default fast path avoids proxy-dependent package downloads after cache warm-up.

## Files And Ownership

- Local entrypoint: `deploy/fast-update.sh`.
- Backend build definition: `deploy/Dockerfile.backend-hotfix`.
- Server source: `/opt/sub2api-src`.
- Server deployment: `/opt/sub2api`.
- Server Compose override: `/opt/sub2api/docker-compose.override.yml`.
- Cloudflare config: `/etc/cloudflared/config.yml`.

The script must not read, print, copy, or modify API keys, database passwords, administrator credentials, or the existing deployment `.env`.

## Failure Handling

- Exit before deployment if tests, local frontend build, synchronization, or server image build fails.
- Record the exact current image ID before switching.
- Use a bounded health-check loop after recreation.
- If health checks fail, rewrite only the image override to the prior immutable image reference and recreate `sub2api`.
- Keep failed images and build logs for diagnosis; do not prune active or rollback images.

## Verification

- Shell syntax check with `bash -n`.
- Focused channel-monitor Go unit tests with the `unit` build tag.
- A dry-run mode validates paths and prints stages without changing the server.
- A real deployment must end with a healthy container and successful HTTPS checks through `huanvel.com`.

## Non-Goals

- Rebuilding or restarting PostgreSQL, Redis, ChatGPT2API, Mihomo, HAProxy, or Cloudflare Tunnel.
- Changing DNS records during routine application updates.
- Publishing images to a public registry.
- Replacing GitHub Actions; a CI-based image pipeline can be added later behind `--ci`.
