# unstuckarr

Imports the completed downloads Radarr and Sonarr leave in their queues because they
cannot tell what the files are: a RuTracker movie named `Kiki_Delivery_Service.mkv`, a
season pack whose files carry only Russian episode titles. It decides which movie or
episodes each file is and imports it through the application's own manual import, as a
person would from the queue's Manual Import dialog.

## What it imports

A download is looked at when its queue state is `importPending` or `importBlocked`. It is
left alone, and reported, when the queue's status messages give any reason other than
these, which say the application could not tell what the files are:

- unable to parse the file or the download
- unknown movie or series, invalid movie, episodes not selected
- invalid season or episode
- episode title missing or TBA
- title mismatch, or matched by ID through the grab history, so that automatic import
  is not possible

So "Not an upgrade", "Episode file already imported", "Destination already exists" or a
sample leave the download alone.

Its files are listed with `GET /api/v3/manualimport?downloadId=`. Files that are not
video, sit in an extras folder (`Extras`, `Featurettes`, `Bonus` and the like), are named
as a sample or trailer, or are smaller than `UNSTUCKARR_MIN_SIZE` are skipped. A
`BDMV` or `VIDEO_TS` structure or an `.iso` or `.img` image leaves the download alone. A
file the application raises no rejection against is left to the application; when it is
a movie's feature, or a file of a series download the application has not warned about,
the download is reported as waiting. A file with a rejection outside the list above is
not imported.

**Radarr.** The movie is the one the download was grabbed for. Of several files, the
largest is the feature when it is at least twice the size of the next; otherwise the LLM
is asked which one is, and without an LLM the download is left alone. A download whose
movie another download in the queue is also awaiting import for is left alone.

**Sonarr.** The queue holds one record per episode of a download; they are taken
together. A file may only be matched to an episode the download was grabbed for, and not
to one another download in the queue is awaiting import for. Its episodes are, in order
of preference: those the application matched it to; those named by `S01E02`, `1x02` or
`Сезон 1 Серия 2`; the number in its name as an episode of the season, when the grab is
of one season; that number as an absolute episode number; the air date in its name. A
number is read where the names of the pack's files differ, skipping dates, resolutions,
`5.1` and `H.265`. Two files matched to one episode are both unmatched. Files left
unmatched go to the LLM.

**LLM.** With `TYPESAFE_API_KEY` set, TypeSafe's Jev is asked one choice question per
file, its options the episodes still unmatched and `skip`, or, for a movie, the candidate
files and `none`. An answer outside the options rejects the whole answer; an answer below
0.5 confidence is no match. No request is made once the day's `UNSTUCKARR_DAILY_TOKENS`
are spent; the count is kept in memory and starts afresh on restart and at midnight UTC.

The matched files are then sent to `POST /api/v3/manualimport`, which has the application
evaluate them with their movie or episodes; a file it rejects for a reason outside the
list above is not imported. The quality and languages are the application's for the file,
else those of the grab. When the application can parse neither the file's name nor the
download's title, reprocessing fails (500, "Unable to parse movie info from path") and
the checks it runs, such as whether the file is an upgrade, never run; such a file is
imported only when its movie or none of its episodes has a file yet, with the quality the
grab was parsed as, and is marked `unreprocessed` in the status. With `UNSTUCKARR_APPLY` true the rest are imported with a
`ManualImport` command in `auto` mode, and the command is followed until it finishes, for
at most six hours; until then the download is not looked at again. The command imports
what it is given without checking it again.

Every decision is logged, with its reasons and files.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `RADARR_URL` | | Radarr, without `/api/v3` |
| `RADARR_API_KEY` | | Radarr API key |
| `SONARR_URL` | | Sonarr, without `/api/v3` |
| `SONARR_API_KEY` | | Sonarr API key |
| `UNSTUCKARR_APPLY` | `false` | import; otherwise decide, log and report only |
| `UNSTUCKARR_INTERVAL` | `10m` | how often each queue is looked at, a Go duration |
| `UNSTUCKARR_ADDR` | `:8080` | listen address |
| `UNSTUCKARR_WEBHOOK_TOKEN` | | password of the webhook; unset, there is no webhook |
| `UNSTUCKARR_MIN_SIZE` | `50MB` | smallest file imported: bytes, or with `KB`, `MB`, `GB` (powers of 1000) or `KiB`, `MiB`, `GiB` |
| `TYPESAFE_API_KEY` | | TypeSafe API key; unset, no LLM is asked |
| `UNSTUCKARR_DAILY_TOKENS` | `200000` | TypeSafe tokens spent per UTC day at most |

At least one of Radarr and Sonarr is required, each with both its variables. Traces and
metrics are exported over OTLP as the standard `OTEL_*` variables configure;
`OTEL_SDK_DISABLED=true` turns them off. Logs are JSON on standard error.

## HTTP

| Route | Answers |
|---|---|
| `GET /healthz` | `ok` |
| `GET /status` | JSON: `apply`, the last run of each application with its decision on each download, the imports started recently and their outcome, and the LLM's spending today |
| `POST /webhook?app=radarr` or `?app=sonarr` | the application's webhook; HTTP Basic auth, any username, the password `UNSTUCKARR_WEBHOOK_TOKEN`. A `ManualInteractionRequired` or `Test` event schedules a run of that application now (202); other events are ignored (204) |

Point the webhook connection of each application at `/webhook?app=radarr` or
`/webhook?app=sonarr` with the "Manual Interaction Required" trigger on.

## Running

```sh
go build .
RADARR_URL=http://127.0.0.1:7878 RADARR_API_KEY=... ./unstuckarr
```

The image, `sarak.sh/arr/unstuckarr`, runs `/unstuckarr` as its entrypoint and exposes
port 8080.
