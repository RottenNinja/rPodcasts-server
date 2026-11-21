# Downloader engine

This is optional. Podcast feeds need no engine: the server reads them itself. An engine extends the server to get media that its built-in podcast reader can't, such as a page of videos or a folder of media.

> **At your own risk.** The engine is executed with the same permissions as the server: it can read and change everything the server can, including its database and your feed passwords. Only run an engine whose source you trust, or a script you or your AI wrote and you have read. Do not use random programs or scripts from the internet. Run the server in Docker, as in `docker-compose.yml`, so that the engine runs as the image's unprivileged user and only sees the container and its `/data/state` volume, not the rest of your machine.

## Have your AI build one

This file is written so that an AI coding assistant can build an engine for you. Give it this whole file and say what you want added, for example "an engine for `folder:///myaudio` that lists the audio files in that folder and serves them over HTTP". It has everything the AI needs: the arguments each touchpoint gets, the JSON it must print, and the settings to set. Then read what it wrote before you run it.

## How it works

The engine is one program, `PODCASTS_ENGINE`, run with arguments at each touchpoint below. A script of your own can stand in for it, for example to read a folder of your own, as long as it takes the arguments you give it and prints the JSON below. To handle several sources, write one wrapper that picks by address.

Addresses with a scheme other than `http(s)`, such as `folder:///audiofiles` or `example://server/library/3`, can be added by hand. They are never fetched as feeds: they go to the engine as written, and only it knows what they mean. A media server at home is usually on your own network, which the server refuses to reach unless `PODCASTS_ALLOW_LOCAL_NETWORK=true`.

## Arguments

Each `PODCASTS_ENGINE_ARGS_<TOUCHPOINT>` is a list of arguments separated by spaces. There is no shell and no quoting, so an argument can't contain a space. The placeholders in braces are filled in, also inside a longer argument. Leave a variable unset to keep its default. Setting artwork, version or update to empty skips that touchpoint, and the server won't start with a placeholder a touchpoint doesn't fill.

| Touchpoint | Variable | Placeholders | Default |
|---|---|---|---|
| list a page's episodes | `PODCASTS_ENGINE_ARGS_LIST` | `{url}` `{limit}` | `list {url} {limit}` |
| the page's own picture | `PODCASTS_ENGINE_ARGS_ARTWORK` | `{url}` | `artwork {url}` |
| one episode's details | `PODCASTS_ENGINE_ARGS_VIDEO` | `{url}` | `video {url}` |
| an episode's media links | `PODCASTS_ENGINE_ARGS_RESOLVE` | `{url}` `{quality}` `{format}` | `resolve {url} {quality}` |
| version | `PODCASTS_ENGINE_ARGS_VERSION` | | `version` |
| self-update, once a day on first use | `PODCASTS_ENGINE_ARGS_UPDATE` | | `update` |

With the defaults, a script switches on its first argument. A script with nothing to do for artwork or update can exit 0 without printing, or the variable can be set to empty.

- `{url}` is the channel's address for list and artwork, and an episode's `url` from the list for video and resolve.
- `{limit}` is how many to list, with some to spare. Listing more is fine: the newest are kept.
- `{quality}` is `audio`, `480`, `720` or `1080`.
- `{format}` is the same quality as a format-selector expression, for an existing tool that takes one.

With `PODCASTS_ENGINE_URL` set and the engine missing, the server downloads that one file to `PODCASTS_ENGINE`, makes it executable and runs it.

## Output

Every touchpoint but update prints JSON on standard output. Fields not listed are ignored and every field is optional, except where noted. A non-zero exit is a failure, and the last line on standard error is shown as the reason, without a leading `ERROR: ` or `[tag] `.

**List** prints one JSON object per line, one per episode, newest first. If list exits non-zero but already printed episodes, the episodes are kept.

```json
{"id": "abc", "url": "example://server/item/abc", "title": "Episode 1", "duration": 1520,
 "timestamp": 1767225600, "thumbnail": "https://…/thumb.jpg",
 "playlist_title": "My Show", "playlist_uploader": "Author", "playlist_webpage_url": "https://…", "playlist_count": 42}
```

- `url` (or `webpage_url`) is required: it is what video and resolve get as `{url}`.
- `id` identifies the episode, falling back to `url`.
- The date is `timestamp` or `release_timestamp` (Unix seconds), or `upload_date` (`YYYYMMDD`).
- `thumbnails: [{"url"}]` works instead of `thumbnail`, with the best last.
- The channel's name and author are taken from the first line: `playlist_title`, `playlist_channel` or `channel`, and `playlist_uploader` or `uploader`.

**Artwork** prints one object: `{"thumbnails": [{"url", "width", "height", "preference"}]}`. The largest square one wins, otherwise the last. A negative `preference` is skipped.

**Video** prints one object: `title`, the date fields as in list, `duration`, `description`, `thumbnail` and `chapters: [{"start_time", "title"}]`. It also says whether the episode has a picture: `vcodec` (`"none"` for sound only), `acodec`, `ext` and `height`, either at the top or in each of `formats: [...]`. With no picture anywhere, the channel's episodes are audio.

**Resolve** prints the video fields plus the media to play, either:
- one file: `url`, `ext`, `vcodec`, `acodec`, `filesize` (or `filesize_approx`), `http_headers: {…}`, `protocol` and `downloader_options: {"http_chunk_size"}` at the top, or
- video and audio to join: `requested_formats`, exactly two objects of those fields, one with a `vcodec` and one with `vcodec: "none"`.

Media links must be `http(s)`. The server fetches them itself, or ffmpeg does, with `http_headers` sent along. For a folder, serve it over HTTP and print links to it.

`thumbnail` and artwork links go to browsers and apps as they are, so they must be reachable from them.

**Version** prints one line, shown on the Status page. **Update** only needs to exit 0.
