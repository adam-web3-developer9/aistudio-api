# Production deployment

Repository: https://github.com/adam-web3-developer9/aistudio-api

Push `main`. GitHub Actions verifies the affected contracts, builds a Linux
amd64 image, and publishes a complete prerelease named `deploy-<full commit>`.
No production SSH key, GitHub personal token, or registry credential is stored
in GitHub Actions or required on the server.

The server's `aistudio-api-deploy.timer` checks once per minute. It deploys only
a complete bundle matching current `main`, verifies both file checksums and
image source/revision labels, and runs the verified local image ID. Deployment
is serialized and a newer push cannot cancel an ongoing switch.

The live target is container `new-api`, data directory
`/www/wwwroot/new-api/data`, and loopback port `127.0.0.1:8080`. Runtime secrets
are installed separately in root-only `/opt/aistudio-api/runtime.env`; they
must never be committed. Nginx's owned extension returns 503 while
`/run/aistudio-api-maintenance` exists. Deployments drain asynchronous image
workers, wait for outstanding batch accounting, and allow 150 seconds for HTTP
shutdown before replacing the container.

New H3 reservations require Redis disabled and `BATCH_UPDATE_ENABLED=false`.
The Studio companion protocol must be deployed separately before enabling a
paid H3 channel. Deploying this gateway does not perform paid video generation.

The initial runtime image, container inspection, and old Compose file are
backed up under `/www/backups/aistudio-api/`. Each switch also retains its old
container and configuration under `/opt/aistudio-api/history/`.

**The operator explicitly skipped database backups.** The existing database
stays in place and is never reset or copied by this deployment. Automatic
failure recovery restores the previous container/configuration; it cannot undo
database migrations. If the restored runtime is unhealthy, maintenance remains
enabled for manual recovery.

Inspect deployment:

```sh
systemctl status aistudio-api-deploy.timer
journalctl -u aistudio-api-deploy.service -n 80 --no-pager
cat /opt/aistudio-api/deployed.json
```

Pause future deployments with `systemctl stop aistudio-api-deploy.timer`.
Manual rollout retries use `systemctl start aistudio-api-deploy.service`.
To change the deployment agent or units themselves, install the reviewed files
from `scripts/deploy/` on the server; application pushes do not overwrite the
root-owned deployment agent.
