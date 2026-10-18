# First-time Ubuntu VPS setup

This runbook prepares the existing Ubuntu server at `<VPS_IP>` to deploy
Next Game Crush at `https://nextgamecrush.site`. Run VPS commands from an
existing administrator account with `sudo`; run commands marked "workstation"
on a trusted local machine. The instructions keep the existing Nginx TLS
certificate and do not expose PostgreSQL or Mailpit.

Before starting, confirm that the DNS `A` record for `nextgamecrush.site`
resolves to `<VPS_IP>` and that the existing site is reachable over HTTPS.

## 1. Create the deploy user

On the VPS, update installed packages and create a dedicated account:

```bash
sudo apt-get update
sudo apt-get upgrade
sudo adduser --disabled-password --gecos '' ngc-deploy
```

The account is used only by GitHub Actions. Do not share it with application
users. It does not need general `sudo` access.

## 2. Install Docker from the official repository

Remove conflicting packages if they are present, then install Docker Engine
and the Compose plugin from Docker's official Ubuntu apt repository:

```bash
conflicting_packages=()
for package in docker.io docker-doc docker-compose docker-compose-v2 docker-buildx podman-docker containerd runc; do
  if dpkg-query -W -f='${db:Status-Status}\n' "$package" 2>/dev/null | grep -qx installed; then
    conflicting_packages+=("$package")
  fi
done
if ((${#conflicting_packages[@]})); then
  sudo apt-get remove -y "${conflicting_packages[@]}"
fi
unset conflicting_packages package
sudo apt-get update
sudo apt-get install -y ca-certificates curl
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
sudo tee /etc/apt/sources.list.d/docker.sources >/dev/null <<EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: $(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/docker.asc
EOF
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo systemctl enable --now docker
sudo docker run --rm hello-world
sudo docker compose version
```

Add the deploy user to the Docker group and start a new login session before
testing it:

```bash
sudo usermod -aG docker ngc-deploy
sudo -iu ngc-deploy docker version
```

Membership in the `docker` group is effectively root-level host access. Use a
dedicated key, keep the account single-purpose, and remove its access when the
deployment is retired.

## 3. Create and authorize a dedicated SSH key

On the trusted workstation, create a key with no passphrase. GitHub Actions
cannot answer an interactive passphrase prompt:

```bash
ssh-keygen -t ed25519 -f ./next-game-crush-deploy -C 'github-actions-next-game-crush'
scp ./next-game-crush-deploy.pub ubuntu@<VPS_IP>:/tmp/next-game-crush-deploy.pub
```

Replace `ubuntu` if the existing administrator account has another name. On
the VPS, install only the public half:

```bash
sudo install -d -o ngc-deploy -g ngc-deploy -m 0700 /home/ngc-deploy/.ssh
sudo install -o ngc-deploy -g ngc-deploy -m 0600 /tmp/next-game-crush-deploy.pub /home/ngc-deploy/.ssh/authorized_keys
sudo rm -f /tmp/next-game-crush-deploy.pub
```

Back on the workstation, verify that the dedicated key works:

```bash
ssh -i ./next-game-crush-deploy -o IdentitiesOnly=yes ngc-deploy@<VPS_IP> 'docker version'
```

Keep `next-game-crush-deploy` private. The `.pub` file is not secret.

## 4. Verify and capture the SSH host fingerprint

Do not trust a host key merely because `ssh-keyscan` returned it. First, read
the Ed25519 fingerprint through the already trusted administrator session:

```bash
sudo ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

Then, on the workstation, capture the key for the same value that will be used
as `VPS_HOST` and display its fingerprint:

```bash
ssh-keyscan -t ed25519 <VPS_IP> > ./next-game-crush-known-hosts
ssh-keygen -lf ./next-game-crush-known-hosts
```

The two SHA256 fingerprints must match exactly. Stop if they differ. Keep the
entire line from `next-game-crush-known-hosts`; it will become the
`VPS_KNOWN_HOSTS` GitHub secret. The workflow uses strict host-key checking and
does not learn keys dynamically.

## 5. Configure UFW

Allow SSH before enabling the firewall so the current session is not locked
out. The existing Nginx profile opens only HTTP and HTTPS:

```bash
sudo ufw allow 22/tcp
sudo ufw allow 'Nginx Full'
sudo ufw enable
sudo ufw status verbose
```

The resulting inbound rules should cover only ports `22`, `80`, and `443`
(plus matching IPv6 rules when IPv6 is enabled). Do not add `8080`, `5432`,
`8025`, or `1025`: the app binds to loopback and the other services have no
host ports. Docker-published ports can bypass ordinary UFW policy, so this
Compose topology—not an extra firewall rule—is what keeps PostgreSQL and
Mailpit off the host network.

## 6. Create the deployment directories

On the VPS:

```bash
sudo install -d -o ngc-deploy -g ngc-deploy -m 0750 /opt/next-game-crush
sudo install -d -o ngc-deploy -g ngc-deploy -m 0750 /opt/next-game-crush/backups
sudo install -d -o ngc-deploy -g ngc-deploy -m 0700 /opt/next-game-crush/incoming
sudo -iu ngc-deploy touch /opt/next-game-crush/deploy.lock
```

GitHub Actions atomically installs `compose.prod.yml` and `deploy.sh` into this
directory. The workflow never overwrites `.env` or the backup.

## 7. Create the protected .env

Open a login shell as the deploy user and collect the real Steam values without
putting them in shell history. Each cryptographic secret below is generated
independently and uses URL-safe hexadecimal characters:

```bash
sudo -iu ngc-deploy
```

At the new `ngc-deploy` prompt, run:

```bash
set -Eeuo pipefail
cd /opt/next-game-crush
umask 077
readonly env_file=/opt/next-game-crush/.env
[[ ! -e "$env_file" && ! -L "$env_file" ]] || {
  printf '%s\n' 'Refusing to replace an existing or symlinked .env' >&2
  exit 1
}
env_tmp=''
cleanup_env_tmp() {
  if [[ -n "$env_tmp" ]]; then
    rm -f -- "$env_tmp"
  fi
}
trap cleanup_env_tmp EXIT
postgres_password="$(openssl rand -hex 32)"
email_action_secret="$(openssl rand -hex 32)"
session_secret="$(openssl rand -hex 32)"
[[ "$postgres_password" =~ ^[0-9a-f]{64}$ && "$email_action_secret" =~ ^[0-9a-f]{64}$ && "$session_secret" =~ ^[0-9a-f]{64}$ ]] || {
  printf '%s\n' 'Secret generation failed validation' >&2
  exit 1
}
read -r -p 'Allowed 17-digit SteamID: ' allowed_steam_id
read -r -s -p 'Steam Web API key: ' steam_web_api_key
printf '\n'
read -r -s -p 'OpenAI API key (press Enter to disable enrichment): ' openai_api_key
printf '\n'
[[ "$allowed_steam_id" =~ ^[0-9]{17}$ ]] || {
  printf '%s\n' 'SteamID must contain exactly 17 digits' >&2
  exit 1
}
[[ -n "$steam_web_api_key" ]] || {
  printf '%s\n' 'Steam Web API key must not be empty' >&2
  exit 1
}
env_tmp="$(mktemp /opt/next-game-crush/.env.tmp.XXXXXX)"
chmod 600 "$env_tmp"
{
  printf 'APP_BASE_URL=https://nextgamecrush.site\n'
  printf 'WORKER_CONCURRENCY=10\n'
  printf 'CATALOG_DISCOVERY_ENABLED=0\n'
  printf 'ALLOWED_STEAM_ID=%s\n' "$allowed_steam_id"
  printf 'STEAM_WEB_API_KEY=%s\n' "$steam_web_api_key"
  printf 'OPENAI_API_KEY=%s\n' "$openai_api_key"
  printf 'OPENAI_MODEL=gpt-6-luna\n'
  printf 'OPENAI_EMBEDDING_MODEL=text-embedding-3-small\n'
  printf 'POSTGRES_DB=next_game_crush\n'
  printf 'POSTGRES_USER=next_game_crush\n'
  printf 'POSTGRES_PASSWORD=%s\n' "$postgres_password"
  printf 'DATABASE_URL=postgres://next_game_crush:%s@db:5432/next_game_crush?sslmode=disable\n' "$postgres_password"
  printf 'SMTP_URL=smtp://mailpit:1025\n'
  printf 'EMAIL_ACTION_SECRET=%s\n' "$email_action_secret"
  printf 'SESSION_SECRET=%s\n' "$session_secret"
  printf 'TEST_HARNESS_MODE=0\n'
} > "$env_tmp"
mv -fT -- "$env_tmp" "$env_file"
env_tmp=
trap - EXIT
unset postgres_password email_action_secret session_secret allowed_steam_id steam_web_api_key openai_api_key
stat -c '%a %U:%G %n' "$env_file"
```

This is intentionally a create-once path: it refuses any existing or symlinked
`.env` instead of overwriting or rotating it. All inputs and generated secrets
are ready before a mode-`0600` temporary file is written; the EXIT trap removes
that temporary file on failure, and GNU `mv -fT` installs it atomically. The
final line must report mode `600` and owner `ngc-deploy`. Do not print, copy off
the server, or commit this file. Leave `OPENAI_API_KEY` empty if AI enrichment
is not needed. Mailpit remains an internal SMTP sink with no web route; it can
be replaced with a real server-side SMTP URL later.

Leave the deploy-user shell with `exit` before continuing with administrator
commands.

## 8. Update and validate Nginx

Find the already enabled HTTPS site in `/etc/nginx/sites-enabled`. Back up its
source file before editing it; substitute the actual filename below if it is
not `nextgamecrush.site`:

```bash
sudo cp --preserve=all /etc/nginx/sites-available/nextgamecrush.site /etc/nginx/sites-available/nextgamecrush.site.before-next-game-crush
sudoedit /etc/nginx/sites-available/nextgamecrush.site
```

In the existing `server` block for `nextgamecrush.site` on port `443`, replace
the old `location /` with the proxy settings from
[`deploy/nginx.conf.example`](../deploy/nginx.conf.example). Keep its current
`ssl_certificate`, `ssl_certificate_key`, Certbot-managed text, and separate
HTTP-to-HTTPS redirect unchanged. There must be only one enabled HTTPS server
block for this hostname.

Validate the complete configuration before reloading:

```bash
sudo nginx -t
sudo systemctl reload nginx
sudo systemctl status nginx --no-pager
```

At this point a `502 Bad Gateway` is expected until the first app deployment;
TLS itself should remain valid.

## 9. Configure the GitHub production Environment

In the public GitHub repository, open **Settings → Environments → New
environment**, create an environment named exactly `production`, and add:

| Kind | Name | Value |
| --- | --- | --- |
| Environment secret | `VPS_SSH_KEY` | Full contents of the private `next-game-crush-deploy` key |
| Environment secret | `VPS_KNOWN_HOSTS` | Reviewed full line from `next-game-crush-known-hosts` |
| Environment variable | `VPS_HOST` | `<VPS_IP>` |
| Environment variable | `VPS_USER` | `ngc-deploy` |

Do not put application secrets in GitHub. The repository workflow needs
permission to create packages: in **Settings → Actions → General → Workflow
permissions**, allow read and write permissions if repository policy currently
blocks package publication. The workflow still declares narrow permissions per
job.

## 10. Publish the first GHCR package and make it public

The package does not exist until the `publish` job succeeds once. Push or merge
a reviewed commit to `main`, then watch **Actions → Verify and deploy**. The
`verify` and `publish` jobs should finish; the first `deploy` may fail because
new GHCR packages are private by default.

After publication, open the `next-game-crush` package from the repository or
profile, choose **Package settings → Danger Zone → Change visibility**, and
make it **Public**. GitHub warns that this visibility change cannot be undone;
this runbook assumes the approved public-package design. Confirm that the
package is linked to this repository. A public package lets the VPS pull
immutable images without storing a long-lived registry token.

## 11. Run the first deployment

In the failed workflow run, use **Re-run jobs → Re-run failed jobs**. If no job
failed, push the next reviewed commit to `main`; the workflow intentionally has
no manual deployment trigger. The deploy job uploads the production Compose
file and deploy script, starts PostgreSQL and internal Mailpit, writes the first
successful dump, pulls the exact commit-SHA image, and checks public health.

Check the result from the VPS without reading `.env`:

```bash
sudo -iu ngc-deploy docker ps --filter label=com.docker.compose.project=next-game-crush
curl --fail --silent --show-error https://nextgamecrush.site/api/health
```

## 12. Check login and logs

Open `https://nextgamecrush.site` in a browser and sign in with the allowlisted
Steam account. A different SteamID must be rejected. Inspect only sanitized
container logs; do not paste them publicly before reviewing them:

```bash
sudo -iu ngc-deploy docker ps --filter label=com.docker.compose.project=next-game-crush
app_container="$(sudo -iu ngc-deploy docker ps -q --filter label=com.docker.compose.project=next-game-crush --filter label=com.docker.compose.service=app)"
sudo -iu ngc-deploy docker logs --tail 200 "$app_container"
sudo journalctl -u nginx --since '15 minutes ago' --no-pager
```

Operational commands and failure diagnosis are in
[`docs/deployment.md`](deployment.md).

## 13. Verify reboot survival

Schedule this check for a maintenance window. A reboot interrupts the site:

```bash
sudo reboot
```

Reconnect after the VPS returns, then confirm Docker, Nginx, all three
containers, and public health recovered:

```bash
sudo systemctl is-active docker nginx
sudo -iu ngc-deploy docker ps --filter label=com.docker.compose.project=next-game-crush
curl --fail --silent --show-error https://nextgamecrush.site/api/health
```

All Compose services use `restart: unless-stopped`, so they should start when
Docker starts. If a container is missing or unhealthy, follow the diagnosis
runbook before triggering another deployment.
