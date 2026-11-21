# rPodcasts server

The self-hosted server for [rPodcasts](https://rpodcasts.rotten.ninja), a podcast and video-podcast player. It keeps your subscriptions, playlists and play progress, serves a web UI, and syncs with the rPodcasts app. It stores no media: episodes play straight from their publishers.

## Run it

Copy `docker-compose.yml`, which lists every setting with a comment, and fill in the password and master key:

```sh
docker compose run --rm rPodcasts password   # prints a fresh PODCASTS_PASSWORD and PODCASTS_MASTER_KEY
docker compose up -d --build                # http://localhost:8484
```

To skip the build, replace `build: .` in `docker-compose.yml` with `image: ghcr.io/rottenninja/rpodcasts-server:latest`.

Without Docker, download the binary for Linux, Windows or macOS from the [latest release](https://github.com/RottenNinja/rPodcasts-server/releases/latest), or build it with `go build ./cmd/podcasts` (CGO is needed for SQLite). Put `ffmpeg` on `PATH`, then run `podcasts serve`.

| Env | Default | |
|---|---|---|
| `PODCASTS_PASSWORD` | required | sign-in password |
| `PODCASTS_MASTER_KEY` | required | 32 bytes, hex or base64. It signs sessions and stream links and encrypts feed passwords |
| `PODCASTS_ADDR` | `:8484` | |
| `PODCASTS_DB` | `./state/podcasts.db` | |
| `PODCASTS_SECURE_COOKIE` | `false` | marks the session cookie HTTPS-only always; without it, only when the request came over HTTPS |
| `PODCASTS_ALLOW_LOCAL_NETWORK` | `false` | lets the server reach private and loopback addresses, such as a media server at home |

**On the public internet**: the server speaks plain HTTP, so put it behind a reverse proxy that does HTTPS (Caddy, nginx, Traefik) and passes `Host`, `X-Forwarded-For` and `X-Forwarded-Proto`, and publish the port to that proxy only. Use the generated password: guessing is slowed down (10 wrong tries per address, then one a minute), which only helps a password that isn't guessable.

Everything else is set in the web UI under Settings → Server.

Then install the rPodcasts app and sign in with your server's address and password.

## Your own downloader engine

Optional. An engine extends the server to get media that the built-in podcast reader can't, such as a page of videos or a folder of media. A script of your own can stand in for it: [ENGINE.md](ENGINE.md) lists the arguments it gets at each touchpoint and the JSON it must print. It is written for your AI: give it ENGINE.md, say what you want added, and it can build you an engine for it.

The engine runs with the same permissions as the server, so use it at your own risk. Only run an engine you trust, never a random program or script from the internet, and run the server in Docker so the engine only sees the container.

## Contributing: send a Prompt Request

Do not send pull requests. Send a prompt request! PR now stands for Prompt Request lol.

Why: me having to spend credits getting my AI to read hundreds of lines of code that you spent credits getting your AI to write, to then tell me what it does, and then having to get my AI to check and validate it, is slow and expensive. Just [open an issue](https://github.com/RottenNinja/rPodcasts-server/issues) saying in plain English what you want, in a single paragraph, and I'll get my AI to write it using the rules it already has. Much simpler, cheaper and better for the planet.

## Licence

Copyright 2026 rotten.ninja. This server is free software under the [GNU General Public License v3.0](LICENSE).
