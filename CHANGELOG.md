# Changelog

## 1.2.0 (2026-11-21)

- Hide played episodes is on for the built-in playlists of a new library, and for a playlist created by an app that doesn't send the setting; existing playlists keep theirs.
- Addresses with schemes other than `http(s)`, such as `folder://…`, can be added by hand and go straight to the downloader engine.
- New `PODCASTS_ALLOW_LOCAL_NETWORK` (default `false`) lets feeds, the downloader engine and ffmpeg reach private and loopback addresses, such as a media server at home.
- The Playlists page has a Find box at the top that narrows the list by name as you type.
- Episodes has a sound-wave icon instead of a magnifying glass, so it no longer looks like search.
- The Podcasts page has a Find box at the top that narrows the grid by name as you type.
- New playlist setting, Hide played episodes (on for new playlists, off for existing ones): the page lists only what is left to play, with Show played under it, while played episodes stay in the playlist and out of `!$playlist`. Synced as `hide_played`; apps that don't send it keep the saved value.
- Add a channel is now Search (first) or Manual, then a preview of the channel with its tags, then Follow; search results no longer open in place.
- An address is read before it is followed, and refused with the reason when it can't be reached or has nothing to play. New `POST /api/v1/preview` and `POST /api/v1/follow`.
- The preview has a Name field, so a channel can be renamed before it is followed (`title` on `POST /api/v1/follow`).
- A channel's name changed in its settings, or when following, is no longer replaced by the next read; emptying it goes back to the address's own name.
- Deprecated: `POST /api/v1/channels` (follow without the check), kept for apps up to 1.1.1; to be removed after the next app release.
- Episodes of a page read with the downloader engine are marked audio when they have no picture, instead of always video.
- Sign-in throttling: 10 wrong passwords per address, then one try a minute; 100 overall, then one every 5 seconds (HTTP 429).
- Session cookie is marked Secure automatically when the request came over HTTPS; signed-out requests are capped at a 64 KB body.
- The downloader engine and ffmpeg only get web addresses that resolve to public networks; ffmpeg may open only HTTP(S).
- Channel video dates: an approximate date is kept as first seen; refresh opens at most 20 approximately dated videos and never reopens exactly dated ones.
- A podcast's own videos can stream and download through the media cache, re-encoded with ffmpeg when larger than the podcast's video quality, or as audio only; older apps keep playing the enclosure directly.
- Video quality shows in the settings of any podcast that has videos.
- Opening a video no longer replaces its listing picture with one that may not exist; the next refresh brings back the listing's picture for videos already affected.
- New pref, Remember speed for each podcast, and `POST /api/v1/channels/{id}/speed`; apps that don't send the pref keep the saved value.
- With nothing followed yet, the Playlists page shows how to start (follow a channel, what playlists are, tags and downloads) with Add a podcast on it.
- The web UI says podcast everywhere it said channel (Add a podcast, Podcast settings).
- Filters take `$podcast` as another name for `$channel`; it is saved as `$channel`, so older apps still read the playlist.

## 1.1.1 (2026-11-15)
